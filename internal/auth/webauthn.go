// Package auth owns WebAuthn, ceremony, session, and CSRF security boundaries.
package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"
)

const CeremonyLifetimeSeconds = 300

// User is the identity-only view supplied to a WebAuthn implementation.
type User struct {
	ID          domain.UserID
	DisplayName domain.DisplayName
	Credentials []model.Credential
}

// RegistrationResult is the verified credential material safe to persist.
type RegistrationResult struct {
	Credential model.Credential
}

// AuthenticationResult identifies the verified owner and updated credential.
type AuthenticationResult struct {
	UserID     domain.UserID
	Credential model.Credential
	Cloned     bool
}

// UserResolver resolves discoverable credentials by both credential ID and
// authenticator-provided user handle. Implementations must verify ownership.
type UserResolver func(rawCredentialID, userHandle []byte) (User, error)

// WebAuthnEngine is the narrow cryptographic adapter used by application code.
// Session bytes are opaque and must be persisted exactly between begin/finish.
type WebAuthnEngine interface {
	BeginRegistration(User, []byte) (options, session json.RawMessage, err error)
	FinishRegistration(User, json.RawMessage, []byte) (RegistrationResult, error)
	BeginAuthentication([]byte) (options, session json.RawMessage, err error)
	FinishAuthentication(json.RawMessage, []byte, UserResolver) (AuthenticationResult, error)
}

// GoWebAuthn delegates all WebAuthn parsing and cryptographic verification to
// github.com/go-webauthn/webauthn. EndlessFS only supplies policy and storage.
type GoWebAuthn struct {
	inner *wa.WebAuthn
}

func NewGoWebAuthn(rpID, rpName, origin string) (*GoWebAuthn, error) {
	inner, err := wa.New(&wa.Config{
		RPID:                  rpID,
		RPDisplayName:         rpName,
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "invalid WebAuthn relying-party configuration", err)
	}
	return &GoWebAuthn{inner: inner}, nil
}

func (w *GoWebAuthn) BeginRegistration(user User, challenge []byte) (json.RawMessage, json.RawMessage, error) {
	if len(challenge) < 32 {
		return nil, nil, domain.NewError(domain.ErrorInvalid, "WebAuthn challenge requires 256 bits")
	}
	webUser, err := newWebAuthnUser(user)
	if err != nil {
		return nil, nil, err
	}
	creation, session, err := w.inner.BeginRegistration(webUser,
		wa.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		wa.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		}),
		wa.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		return nil, nil, domain.WrapError(domain.ErrorInvalid, "start WebAuthn registration", err)
	}
	creation.Response.Challenge = protocol.URLEncodedBase64(append([]byte(nil), challenge...))
	session.Challenge = creation.Response.Challenge.String()
	session.UserVerification = protocol.VerificationRequired
	return encodeOpaque(creation, session)
}

func (w *GoWebAuthn) FinishRegistration(user User, sessionData json.RawMessage, response []byte) (RegistrationResult, error) {
	webUser, err := newWebAuthnUser(user)
	if err != nil {
		return RegistrationResult{}, err
	}
	var session wa.SessionData
	if err := json.Unmarshal(sessionData, &session); err != nil {
		return RegistrationResult{}, domain.WrapError(domain.ErrorInvalid, "invalid registration ceremony state", err)
	}
	request := credentialRequest(response)
	credential, err := w.inner.FinishRegistration(webUser, session, request)
	if err != nil {
		return RegistrationResult{}, domain.WrapError(domain.ErrorUnauthenticated, "WebAuthn registration verification failed", err)
	}
	record, err := modelCredential(user.ID, credential, domain.CredentialLabel{}, false)
	if err != nil {
		return RegistrationResult{}, err
	}
	return RegistrationResult{Credential: record}, nil
}

func (w *GoWebAuthn) BeginAuthentication(challenge []byte) (json.RawMessage, json.RawMessage, error) {
	if len(challenge) < 32 {
		return nil, nil, domain.NewError(domain.ErrorInvalid, "WebAuthn challenge requires 256 bits")
	}
	assertion, session, err := w.inner.BeginDiscoverableLogin(
		wa.WithChallenge(append([]byte(nil), challenge...)),
		wa.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, nil, domain.WrapError(domain.ErrorInvalid, "start WebAuthn authentication", err)
	}
	session.UserVerification = protocol.VerificationRequired
	return encodeOpaque(assertion, session)
}

func (w *GoWebAuthn) FinishAuthentication(sessionData json.RawMessage, response []byte, resolve UserResolver) (AuthenticationResult, error) {
	var session wa.SessionData
	if err := json.Unmarshal(sessionData, &session); err != nil {
		return AuthenticationResult{}, domain.WrapError(domain.ErrorInvalid, "invalid authentication ceremony state", err)
	}
	var resolved User
	webUser, credential, err := w.inner.FinishPasskeyLogin(func(rawID, userHandle []byte) (wa.User, error) {
		user, resolveErr := resolve(rawID, userHandle)
		if resolveErr != nil {
			return nil, resolveErr
		}
		resolved = user
		return newWebAuthnUser(user)
	}, session, credentialRequest(response))
	if err != nil {
		return AuthenticationResult{}, domain.WrapError(domain.ErrorUnauthenticated, "WebAuthn authentication verification failed", err)
	}
	if webUser == nil || !resolved.ID.Valid() || !bytes.Equal(webUser.WebAuthnID(), mustUserIDBytes(resolved.ID)) {
		return AuthenticationResult{}, domain.NewError(domain.ErrorUnauthenticated, "WebAuthn owner resolution failed")
	}
	record, err := modelCredential(resolved.ID, credential, domain.CredentialLabel{}, true)
	if err != nil {
		return AuthenticationResult{}, err
	}
	return AuthenticationResult{UserID: resolved.ID, Credential: record, Cloned: credential.Authenticator.CloneWarning}, nil
}

type webAuthnUser struct {
	id          []byte
	name        string
	displayName string
	credentials []wa.Credential
}

func newWebAuthnUser(user User) (*webAuthnUser, error) {
	if !user.ID.Valid() || user.DisplayName.String() == "" {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid WebAuthn user")
	}
	credentials := make([]wa.Credential, 0, len(user.Credentials))
	for _, record := range user.Credentials {
		credential, err := libraryCredential(record)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return &webAuthnUser{
		id:          mustUserIDBytes(user.ID),
		name:        user.ID.String(),
		displayName: user.DisplayName.String(),
		credentials: credentials,
	}, nil
}

func (u *webAuthnUser) WebAuthnID() []byte          { return append([]byte(nil), u.id...) }
func (u *webAuthnUser) WebAuthnName() string        { return u.name }
func (u *webAuthnUser) WebAuthnDisplayName() string { return u.displayName }
func (u *webAuthnUser) WebAuthnCredentials() []wa.Credential {
	return append([]wa.Credential(nil), u.credentials...)
}

func encodeOpaque(options, session any) (json.RawMessage, json.RawMessage, error) {
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		return nil, nil, domain.WrapError(domain.ErrorInternal, "encode WebAuthn options", err)
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return nil, nil, domain.WrapError(domain.ErrorInternal, "encode WebAuthn ceremony state", err)
	}
	return optionsJSON, sessionJSON, nil
}

func credentialRequest(body []byte) *http.Request {
	request, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func mustUserIDBytes(userID domain.UserID) []byte {
	decoded, _ := base64.RawURLEncoding.DecodeString(userID.String())
	return decoded
}

func libraryCredential(record model.Credential) (wa.Credential, error) {
	id, err := base64.RawURLEncoding.DecodeString(record.CredentialID)
	if err != nil {
		return wa.Credential{}, domain.NewError(domain.ErrorInvalid, "invalid stored credential ID")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(record.PublicKey)
	if err != nil {
		return wa.Credential{}, domain.NewError(domain.ErrorInvalid, "invalid stored credential public key")
	}
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified
	if record.BackupEligible != nil && *record.BackupEligible {
		flags |= protocol.FlagBackupEligible
	}
	if record.BackupState != nil && *record.BackupState {
		flags |= protocol.FlagBackupState
	}
	transports := make([]protocol.AuthenticatorTransport, len(record.Transports))
	for index, transport := range record.Transports {
		transports[index] = protocol.AuthenticatorTransport(transport)
	}
	return wa.Credential{
		ID: id, PublicKey: publicKey, Transport: transports,
		Flags:         wa.NewCredentialFlags(flags),
		Authenticator: wa.Authenticator{SignCount: record.SignCount},
	}, nil
}

func modelCredential(userID domain.UserID, credential *wa.Credential, existingLabel domain.CredentialLabel, retainLabel bool) (model.Credential, error) {
	if credential == nil || len(credential.ID) == 0 || len(credential.PublicKey) == 0 {
		return model.Credential{}, domain.NewError(domain.ErrorInvalid, "verified credential is incomplete")
	}
	backupEligible := credential.Flags.BackupEligible
	backupState := credential.Flags.BackupState
	transports := make([]string, len(credential.Transport))
	for index, transport := range credential.Transport {
		transports[index] = string(transport)
	}
	record := model.Credential{
		SchemaVersion:  model.SchemaVersion,
		CredentialID:   base64.RawURLEncoding.EncodeToString(credential.ID),
		UserID:         userID,
		PublicKey:      base64.RawURLEncoding.EncodeToString(credential.PublicKey),
		SignCount:      credential.Authenticator.SignCount,
		Transports:     transports,
		BackupEligible: &backupEligible,
		BackupState:    &backupState,
	}
	if retainLabel && existingLabel.String() != "" {
		record.Label = &existingLabel
	}
	return record, nil
}
