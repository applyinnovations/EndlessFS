package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestIntegrationIdentityHTTPBootstrapLoginCSRFAndAdmin(t *testing.T) {
	handler, bootstrapToken := identityHTTPHarness(t)
	origin := "https://drive.example.test"

	options := performJSON(t, handler, "POST", "/api/v1/bootstrap/options", origin,
		`{"bootstrapToken":"`+bootstrapToken+`","displayName":"Administrator"}`, nil)
	if options.Code != 200 || options.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("bootstrap options = %d %s", options.Code, options.Body.String())
	}
	ceremonyCookie := responseCookie(t, options, secureCeremonyCookie)
	var optionBody struct {
		CeremonyID string `json:"ceremonyID"`
	}
	decodeResponse(t, options, &optionBody)
	verifyBody := `{"ceremonyID":"` + optionBody.CeremonyID + `","credential":{"seed":41}}`
	verified := performJSON(t, handler, "POST", "/api/v1/bootstrap/verify", origin, verifyBody, []*http.Cookie{ceremonyCookie})
	if verified.Code != 200 {
		t.Fatalf("bootstrap verify = %d %s", verified.Code, verified.Body.String())
	}
	var complete identity.RegistrationComplete
	decodeResponse(t, verified, &complete)
	if !complete.UserID.Valid() || complete.CredentialID == "" || complete.Flow != model.CeremonyBootstrap {
		t.Fatalf("bootstrap result = %+v", complete)
	}

	authOptions := performJSON(t, handler, "POST", "/api/v1/authentication/options", origin, `{}`, nil)
	if authOptions.Code != 200 {
		t.Fatalf("authentication options = %d %s", authOptions.Code, authOptions.Body.String())
	}
	authCeremonyCookie := responseCookie(t, authOptions, secureCeremonyCookie)
	decodeResponse(t, authOptions, &optionBody)
	authBody := `{"ceremonyID":"` + optionBody.CeremonyID + `","credential":{"userID":"` + complete.UserID.String() + `","credentialID":"` + complete.CredentialID + `"}}`
	authenticated := performJSON(t, handler, "POST", "/api/v1/authentication/verify", origin, authBody, []*http.Cookie{authCeremonyCookie})
	if authenticated.Code != 200 {
		t.Fatalf("authentication verify = %d %s", authenticated.Code, authenticated.Body.String())
	}
	sessionCookie := responseCookie(t, authenticated, auth.SecureSessionCookieName)
	csrfCookie := responseCookie(t, authenticated, auth.SecureCSRFCookieName)
	if !sessionCookie.HttpOnly || csrfCookie.HttpOnly || !sessionCookie.Secure || !csrfCookie.Secure {
		t.Fatalf("session/CSRF cookies = %+v / %+v", sessionCookie, csrfCookie)
	}

	me := performRequest(t, handler, "GET", "/api/v1/me", "", "", []*http.Cookie{sessionCookie}, nil)
	if me.Code != 200 || !bytes.Contains(me.Body.Bytes(), []byte(`"roles":["admin"]`)) {
		t.Fatalf("me = %d %s", me.Code, me.Body.String())
	}
	missingCSRF := performJSON(t, handler, "PATCH", "/api/v1/me", origin, `{"displayName":"Renamed"}`, []*http.Cookie{sessionCookie})
	if missingCSRF.Code != 403 {
		t.Fatalf("missing CSRF status = %d", missingCSRF.Code)
	}
	renamed := performRequest(t, handler, "PATCH", "/api/v1/me", origin, `{"displayName":"Renamed"}`, []*http.Cookie{sessionCookie, csrfCookie}, map[string]string{csrfHeader: csrfCookie.Value, "Content-Type": "application/json"})
	if renamed.Code != 200 || !bytes.Contains(renamed.Body.Bytes(), []byte("Renamed")) {
		t.Fatalf("rename = %d %s", renamed.Code, renamed.Body.String())
	}

	withoutIdempotency := performRequest(t, handler, "POST", "/api/v1/admin/invites", origin, `{}`, []*http.Cookie{sessionCookie, csrfCookie}, map[string]string{csrfHeader: csrfCookie.Value, "Content-Type": "application/json"})
	if withoutIdempotency.Code != 400 {
		t.Fatalf("missing idempotency status = %d", withoutIdempotency.Code)
	}
	createdInvite := performRequest(t, handler, "POST", "/api/v1/admin/invites", origin, `{}`, []*http.Cookie{sessionCookie, csrfCookie}, map[string]string{
		csrfHeader: csrfCookie.Value, "Content-Type": "application/json", "Idempotency-Key": "http-invite-test-0001",
	})
	if createdInvite.Code != 201 || !bytes.Contains(createdInvite.Body.Bytes(), []byte("/register/invite/")) {
		t.Fatalf("create invite = %d %s", createdInvite.Code, createdInvite.Body.String())
	}
}

func TestIdentityHTTPRejectsOriginAndMalformedJSONWithStableProblem(t *testing.T) {
	handler, bootstrapToken := identityHTTPHarness(t)
	wrongOrigin := performJSON(t, handler, "POST", "/api/v1/bootstrap/options", "https://evil.example", `{}`, nil)
	if wrongOrigin.Code != 403 {
		t.Fatalf("wrong origin status = %d", wrongOrigin.Code)
	}
	assertProblem(t, wrongOrigin)

	for name, body := range map[string]string{
		"unknown":   `{"bootstrapToken":"` + bootstrapToken + `","displayName":"Admin","role":"admin"}`,
		"duplicate": `{"bootstrapToken":"` + bootstrapToken + `","displayName":"Admin","displayName":"Other"}`,
		"trailing":  `{"bootstrapToken":"` + bootstrapToken + `","displayName":"Admin"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := performJSON(t, handler, "POST", "/api/v1/bootstrap/options", "https://drive.example.test", body, nil)
			if response.Code != 400 {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			assertProblem(t, response)
			if strings.Contains(response.Body.String(), bootstrapToken) {
				t.Fatal("problem response leaked bootstrap token")
			}
		})
	}
}

func identityHTTPHarness(t *testing.T) (http.Handler, string) {
	t.Helper()
	store := state.NewMemoryStore()
	repository := identity.NewRepository(store)
	ids := domain.NewIDGenerator(&httpDeterministicReader{next: 1})
	clock := domain.NewFixedClock(time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC))
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, "https://drive.example.test", true, secret.Value(httpBearer(0x66)))
	if err != nil {
		t.Fatal(err)
	}
	bootstrapToken := httpBearer(0x77)
	policy := identity.NewMutablePolicy(identity.RegistrationPolicy{AllowInvite: true})
	service, err := identity.NewService(repository, httpFakeWebAuthn{}, sessions, ids, clock, policy, secret.Value(bootstrapToken), "https://drive.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		BaseURL: "https://drive.example.test", AllowedOrigin: "https://drive.example.test", Secure: true,
		AllowRegistration: false, InviteRegistration: true,
	}
	return NewApplication(cfg, "test", service, sessions), bootstrapToken
}

type httpFakeWebAuthn struct{}

func (httpFakeWebAuthn) BeginRegistration(user auth.User, challenge []byte) (json.RawMessage, json.RawMessage, error) {
	options, _ := json.Marshal(map[string]any{"challenge": base64.RawURLEncoding.EncodeToString(challenge)})
	session, _ := json.Marshal(map[string]string{"userID": user.ID.String()})
	return options, session, nil
}

func (httpFakeWebAuthn) FinishRegistration(user auth.User, session json.RawMessage, response []byte) (auth.RegistrationResult, error) {
	var saved map[string]string
	var submitted struct {
		Seed byte `json:"seed"`
	}
	if json.Unmarshal(session, &saved) != nil || json.Unmarshal(response, &submitted) != nil || saved["userID"] != user.ID.String() || submitted.Seed == 0 {
		return auth.RegistrationResult{}, domain.NewError(domain.ErrorUnauthenticated, "verification failed")
	}
	return auth.RegistrationResult{Credential: model.Credential{
		CredentialID: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{submitted.Seed}, 32)),
		PublicKey:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{submitted.Seed ^ 0xff}, 32)),
	}}, nil
}

func (httpFakeWebAuthn) BeginAuthentication(challenge []byte) (json.RawMessage, json.RawMessage, error) {
	options, _ := json.Marshal(map[string]any{"challenge": base64.RawURLEncoding.EncodeToString(challenge)})
	return options, json.RawMessage(`{"type":"authentication"}`), nil
}

func (httpFakeWebAuthn) FinishAuthentication(_ json.RawMessage, response []byte, resolve auth.UserResolver) (auth.AuthenticationResult, error) {
	var submitted struct {
		UserID       string `json:"userID"`
		CredentialID string `json:"credentialID"`
	}
	if err := json.Unmarshal(response, &submitted); err != nil {
		return auth.AuthenticationResult{}, err
	}
	userID, err := domain.ParseUserID(submitted.UserID)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	rawID, err := base64.RawURLEncoding.DecodeString(submitted.CredentialID)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	handle, _ := base64.RawURLEncoding.DecodeString(userID.String())
	user, err := resolve(rawID, handle)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	for _, credential := range user.Credentials {
		if credential.CredentialID == submitted.CredentialID {
			return auth.AuthenticationResult{UserID: user.ID, Credential: credential}, nil
		}
	}
	return auth.AuthenticationResult{}, domain.NewError(domain.ErrorUnauthenticated, "verification failed")
}

type httpDeterministicReader struct {
	mu   sync.Mutex
	next byte
}

func (r *httpDeterministicReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range buffer {
		buffer[index] = r.next
		r.next++
		if r.next == 0 {
			r.next = 1
		}
	}
	return len(buffer), nil
}

func httpBearer(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func performJSON(t *testing.T, handler http.Handler, method, path, origin, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return performRequest(t, handler, method, path, origin, body, cookies, map[string]string{"Content-Type": "application/json"})
}

func performRequest(t *testing.T, handler http.Handler, method, path, origin, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func responseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("response did not set cookie %q: %v", name, response.Result().Cookies())
	return nil
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatal(err)
	}
}

func assertProblem(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Header().Get("Content-Type") != "application/problem+json" || response.Header().Get("X-Request-ID") == "" {
		t.Fatalf("problem headers = %v", response.Header())
	}
	var value problem
	decodeResponse(t, response, &value)
	if value.Status != response.Code || value.Code == "" || value.RequestID == "" || value.Type == "" || value.Title == "" {
		t.Fatalf("problem = %+v", value)
	}
}
