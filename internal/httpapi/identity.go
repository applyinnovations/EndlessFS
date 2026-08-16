package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/theme"
)

const (
	secureCeremonyCookie = "__Host-endlessfs_ceremony"
	devCeremonyCookie    = "endlessfs_ceremony_dev"
	csrfHeader           = "X-EndlessFS-CSRF"
	maxControlBodyBytes  = 1 << 20
)

type identityAPI struct {
	config   config.Config
	identity *identity.Service
	sessions *auth.SessionManager
	drive    *drive.Service
	themes   *theme.Manager
}

func (api *identityAPI) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/bootstrap/options", api.bootstrapOptions)
	mux.HandleFunc("POST /api/v1/bootstrap/verify", api.registrationVerify)
	mux.HandleFunc("POST /api/v1/registration/options", api.registrationOptions)
	mux.HandleFunc("POST /api/v1/registration/verify", api.registrationVerify)
	mux.HandleFunc("POST /api/v1/authentication/options", api.authenticationOptions)
	mux.HandleFunc("POST /api/v1/authentication/verify", api.authenticationVerify)
	mux.HandleFunc("POST /api/v1/logout", api.logout)
	mux.HandleFunc("GET /api/v1/me", api.me)
	mux.HandleFunc("PATCH /api/v1/me", api.updateMe)
	mux.HandleFunc("GET /api/v1/me/passkeys", api.passkeys)
	mux.HandleFunc("POST /api/v1/me/passkeys/options", api.passkeyOptions)
	mux.HandleFunc("POST /api/v1/me/passkeys/verify", api.passkeyVerify)
	mux.HandleFunc("DELETE /api/v1/me/passkeys/{credentialID}", api.removePasskey)
	mux.HandleFunc("GET /api/v1/admin/invites", api.adminInvites)
	mux.HandleFunc("POST /api/v1/admin/invites", api.createInvite)
	mux.HandleFunc("DELETE /api/v1/admin/invites/{inviteID}", api.revokeInvite)
	mux.HandleFunc("GET /api/v1/admin/users", api.adminUsers)
	mux.HandleFunc("POST /api/v1/admin/users/{userID}/disable", api.disableUser)
	mux.HandleFunc("POST /api/v1/admin/users/{userID}/enable", api.enableUser)
	mux.HandleFunc("POST /api/v1/admin/users/{userID}/admin", api.grantAdmin)
	mux.HandleFunc("DELETE /api/v1/admin/users/{userID}/admin", api.revokeAdmin)
	mux.HandleFunc("POST /api/v1/admin/users/{userID}/recoveries", api.createRecovery)
	mux.HandleFunc("POST /api/v1/recovery/options", api.recoveryOptions)
	mux.HandleFunc("POST /api/v1/recovery/verify", api.registrationVerify)
	if api.drive != nil {
		api.driveRoutes(mux)
	}
	if api.themes != nil {
		api.themeRoutes(mux)
	}
}

func (api *identityAPI) bootstrapOptions(w http.ResponseWriter, r *http.Request) {
	if !api.requireOrigin(w, r) {
		return
	}
	var request struct {
		BootstrapToken string `json:"bootstrapToken"`
		DisplayName    string `json:"displayName"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	start, err := api.identity.StartBootstrap(r.Context(), secret.Value(request.BootstrapToken), request.DisplayName)
	api.writeCeremony(w, r, start, err)
}

func (api *identityAPI) registrationOptions(w http.ResponseWriter, r *http.Request) {
	if !api.requireOrigin(w, r) {
		return
	}
	var request struct {
		DisplayName string `json:"displayName"`
		InviteToken string `json:"inviteToken,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	start, err := api.identity.StartRegistration(r.Context(), identity.RegistrationStartRequest{
		DisplayName: request.DisplayName, InviteToken: secret.Value(request.InviteToken), ClientKey: r.RemoteAddr,
	})
	api.writeCeremony(w, r, start, err)
}

func (api *identityAPI) recoveryOptions(w http.ResponseWriter, r *http.Request) {
	if !api.requireOrigin(w, r) {
		return
	}
	var request struct {
		RecoveryToken string `json:"recoveryToken"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	start, err := api.identity.StartRecovery(r.Context(), secret.Value(request.RecoveryToken))
	api.writeCeremony(w, r, start, err)
}

func (api *identityAPI) registrationVerify(w http.ResponseWriter, r *http.Request) {
	if !api.requireOrigin(w, r) {
		return
	}
	var request ceremonyVerifyRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	binding, ok := api.ceremonyBinding(w, r)
	if !ok {
		return
	}
	complete, err := api.identity.VerifyRegistration(r.Context(), request.CeremonyID, binding, request.Credential)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	api.clearCeremonyCookie(w)
	writeJSON(w, http.StatusOK, complete)
}

func (api *identityAPI) authenticationOptions(w http.ResponseWriter, r *http.Request) {
	if !api.requireOrigin(w, r) {
		return
	}
	if !decodeEmptyJSON(w, r) {
		return
	}
	start, err := api.identity.StartAuthentication(r.Context())
	api.writeCeremony(w, r, start, err)
}

func (api *identityAPI) authenticationVerify(w http.ResponseWriter, r *http.Request) {
	if !api.requireOrigin(w, r) {
		return
	}
	var request ceremonyVerifyRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	binding, ok := api.ceremonyBinding(w, r)
	if !ok {
		return
	}
	issued, err := api.identity.VerifyAuthentication(r.Context(), request.CeremonyID, binding, request.Credential)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	api.clearCeremonyCookie(w)
	http.SetCookie(w, api.sessions.Cookie(issued))
	http.SetCookie(w, api.sessions.CSRFCookie(issued))
	writeJSON(w, http.StatusOK, map[string]any{"userID": issued.Record.UserID})
}

func (api *identityAPI) logout(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	if err := api.sessions.Logout(r.Context(), current); err != nil {
		writeProblem(w, r, err)
		return
	}
	http.SetCookie(w, api.sessions.ClearCookie())
	http.SetCookie(w, api.sessions.ClearCSRFCookie())
	w.WriteHeader(http.StatusNoContent)
}

func (api *identityAPI) me(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	user, err := api.identity.CurrentUser(r.Context(), current)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (api *identityAPI) updateMe(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request struct {
		DisplayName string `json:"displayName"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	profile, err := api.identity.UpdateDisplayName(r.Context(), current, request.DisplayName)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (api *identityAPI) passkeys(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	passkeys, err := api.identity.Passkeys(r.Context(), current)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": passkeys})
}

func (api *identityAPI) passkeyOptions(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Label string `json:"label,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	start, err := api.identity.StartAddPasskey(r.Context(), current, request.Label)
	api.writeCeremony(w, r, start, err)
}

func (api *identityAPI) passkeyVerify(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request ceremonyVerifyRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	binding, ok := api.ceremonyBinding(w, r)
	if !ok {
		return
	}
	complete, err := api.identity.VerifyRegistration(r.Context(), request.CeremonyID, binding, request.Credential)
	if err != nil || complete.Flow != model.CeremonyAddPasskey || complete.UserID != current.Record.UserID {
		if err == nil {
			err = domain.NewError(domain.ErrorUnauthorized, "passkey ceremony owner mismatch")
		}
		writeProblem(w, r, err)
		return
	}
	issued, err := api.sessions.Rotate(r.Context(), current, complete.CredentialID)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	api.clearCeremonyCookie(w)
	http.SetCookie(w, api.sessions.Cookie(issued))
	http.SetCookie(w, api.sessions.CSRFCookie(issued))
	writeJSON(w, http.StatusOK, complete)
}

func (api *identityAPI) removePasskey(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	if err := api.identity.RemovePasskey(r.Context(), current, r.PathValue("credentialID")); err != nil {
		writeProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *identityAPI) adminInvites(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	invites, err := api.identity.Invites(r.Context(), current)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites})
}

func (api *identityAPI) createInvite(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	created, err := api.identity.CreateInvite(r.Context(), current, request.ExpiresAt, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"inviteID": created.InviteID, "link": created.Link.Reveal(),
		"createdAt": created.Record.CreatedAt, "expiresAt": created.Record.ExpiresAt,
	})
}

func (api *identityAPI) revokeInvite(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	if err := api.identity.RevokeInvite(r.Context(), current, r.PathValue("inviteID")); err != nil {
		writeProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *identityAPI) adminUsers(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	limit := 0
	var err error
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "invalid page limit"))
			return
		}
	}
	page, err := api.identity.AdminUsersPage(r.Context(), current, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *identityAPI) disableUser(w http.ResponseWriter, r *http.Request) {
	api.adminUserMutation(w, r, api.identity.DisableUser)
}

func (api *identityAPI) enableUser(w http.ResponseWriter, r *http.Request) {
	api.adminUserMutation(w, r, api.identity.EnableUser)
}

func (api *identityAPI) grantAdmin(w http.ResponseWriter, r *http.Request) {
	api.adminUserMutation(w, r, api.identity.GrantAdmin)
}

func (api *identityAPI) revokeAdmin(w http.ResponseWriter, r *http.Request) {
	api.adminUserMutation(w, r, api.identity.RevokeAdmin)
}

func (api *identityAPI) adminUserMutation(w http.ResponseWriter, r *http.Request, mutation func(context.Context, auth.AuthenticatedSession, domain.UserID) error) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	userID, err := domain.ParseUserID(r.PathValue("userID"))
	if err == nil {
		err = mutation(r.Context(), current, userID)
	}
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *identityAPI) createRecovery(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	userID, err := domain.ParseUserID(r.PathValue("userID"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	var request struct {
		TTLSeconds int64 `json:"ttlSeconds,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.TTLSeconds == 0 {
		request.TTLSeconds = 900
	}
	created, err := api.identity.CreateRecovery(r.Context(), current, userID, time.Duration(request.TTLSeconds)*time.Second, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"recoveryID": created.RecoveryID, "link": created.Link.Reveal(),
		"targetUserID": created.Record.TargetUserID, "createdAt": created.Record.CreatedAt,
		"expiresAt": created.Record.ExpiresAt,
	})
}

type ceremonyVerifyRequest struct {
	CeremonyID string          `json:"ceremonyID"`
	Credential json.RawMessage `json:"credential"`
}

func (api *identityAPI) writeCeremony(w http.ResponseWriter, r *http.Request, start identity.CeremonyStart, err error) {
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	// #nosec G124 -- Secure is false only for configuration-validated loopback development.
	http.SetCookie(w, &http.Cookie{
		Name: api.ceremonyCookieName(), Value: start.BrowserBinding.Reveal(), Path: "/",
		MaxAge: int(identity.CeremonyLifetime.Seconds()), Secure: api.config.Secure,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	response := map[string]any{"ceremonyID": start.CeremonyID}
	var options map[string]json.RawMessage
	if json.Unmarshal(start.Options, &options) == nil {
		for name, value := range options {
			response[name] = value
		}
	} else {
		response["publicKey"] = start.Options
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *identityAPI) ceremonyBinding(w http.ResponseWriter, r *http.Request) (secret.Value, bool) {
	cookie, err := r.Cookie(api.ceremonyCookieName())
	if err != nil || !secret.ValidBearerToken(cookie.Value) {
		writeProblem(w, r, domain.NewError(domain.ErrorUnauthenticated, "invalid ceremony"))
		return "", false
	}
	return secret.Value(cookie.Value), true
}

func (api *identityAPI) ceremonyCookieName() string {
	if api.config.Secure {
		return secureCeremonyCookie
	}
	return devCeremonyCookie
}

func (api *identityAPI) clearCeremonyCookie(w http.ResponseWriter) {
	// #nosec G124 -- Expiration preserves the dynamic secure-mode attributes of the ceremony cookie it clears.
	http.SetCookie(w, &http.Cookie{
		Name: api.ceremonyCookieName(), Value: "", Path: "/", Expires: time.Unix(1, 0).UTC(),
		MaxAge: -1, Secure: api.config.Secure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (api *identityAPI) authenticated(w http.ResponseWriter, r *http.Request) (auth.AuthenticatedSession, bool) {
	cookie, err := r.Cookie(api.sessions.CookieName())
	if err != nil {
		writeProblem(w, r, domain.NewError(domain.ErrorUnauthenticated, "authentication required"))
		return auth.AuthenticatedSession{}, false
	}
	current, err := api.sessions.Authenticate(r.Context(), cookie.Value)
	if err != nil {
		writeProblem(w, r, err)
		return auth.AuthenticatedSession{}, false
	}
	return current, true
}

func (api *identityAPI) mutation(w http.ResponseWriter, r *http.Request) (auth.AuthenticatedSession, bool) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return auth.AuthenticatedSession{}, false
	}
	if err := api.sessions.AuthorizeMutation(current, r.Header.Get(csrfHeader), r.Header.Get("Origin")); err != nil {
		writeProblem(w, r, err)
		return auth.AuthenticatedSession{}, false
	}
	return current, true
}

func (api *identityAPI) idempotentMutation(w http.ResponseWriter, r *http.Request) (auth.AuthenticatedSession, bool) {
	current, ok := api.mutation(w, r)
	if !ok {
		return auth.AuthenticatedSession{}, false
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 || strings.ContainsAny(key, "\r\n\x00") {
		writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "a valid Idempotency-Key is required"))
		return auth.AuthenticatedSession{}, false
	}
	return current, true
}

func (api *identityAPI) requireOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != api.config.AllowedOrigin {
		writeProblem(w, r, domain.NewError(domain.ErrorUnauthorized, "request origin is not allowed"))
		return false
	}
	return true
}

func decodeEmptyJSON(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	var empty struct{}
	return decodeJSON(w, r, &empty)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	if mediaType := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(mediaType), "application/json") {
		writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "Content-Type must be application/json"))
		return false
	}
	reader := http.MaxBytesReader(w, r.Body, maxControlBodyBytes)
	data, err := io.ReadAll(reader)
	if err != nil || state.DecodeJSONWithLimit(data, destination, maxControlBodyBytes) != nil {
		writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "invalid JSON request"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	RequestID string `json:"requestID"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	kind := domain.KindOf(err)
	status := http.StatusInternalServerError
	title := "Internal server error"
	switch kind {
	case domain.ErrorInvalid:
		status, title = http.StatusBadRequest, "Invalid request"
	case domain.ErrorUnauthenticated:
		status, title = http.StatusUnauthorized, "Authentication required"
	case domain.ErrorUnauthorized:
		status, title = http.StatusForbidden, "Access denied"
	case domain.ErrorNotFound:
		status, title = http.StatusNotFound, "Not found"
	case domain.ErrorConflict:
		status, title = http.StatusConflict, "Conflict"
	case domain.ErrorPreconditionFailed:
		status, title = http.StatusPreconditionFailed, "Precondition failed"
	case domain.ErrorRateLimited:
		status, title = http.StatusTooManyRequests, "Rate limit exceeded"
	case domain.ErrorUnavailable:
		status, title = http.StatusServiceUnavailable, "Unavailable"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type: "https://endlessfs.invalid/problems/" + string(kind), Title: title,
		Status: status, Code: string(kind), RequestID: requestID(r),
	})
}

type requestIDContextKey struct{}

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func requestID(r *http.Request) string {
	value, _ := r.Context().Value(requestIDContextKey{}).(string)
	return value
}

func newRequestID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "request-unavailable"
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := r.Header.Get("X-Request-ID")
		if !validRequestID.MatchString(value) {
			value = newRequestID()
		}
		w.Header().Set("X-Request-ID", value)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, value)))
	})
}
