package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/descope/virtualwebauthn"
	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"
)

func TestIntegrationGoWebAuthnVirtualAuthenticatorUsernamelessFlow(t *testing.T) {
	rp := virtualwebauthn.RelyingParty{Name: "EndlessFS", ID: "drive.example.test", Origin: "https://drive.example.test"}
	adapter, err := NewGoWebAuthn(rp.ID, rp.Name, rp.Origin)
	if err != nil {
		t.Fatal(err)
	}
	userID := testUserID(t, 0x31)
	displayName, _ := domain.ParseDisplayName("Alex Device")
	user := User{ID: userID, DisplayName: displayName}
	challenge := bytes.Repeat([]byte{0x44}, 32)
	options, registrationSession, err := adapter.BeginRegistration(user, challenge)
	if err != nil {
		t.Fatal(err)
	}
	assertRegistrationPolicy(t, options, challenge, userID)
	parsedOptions, err := virtualwebauthn.ParseAttestationOptions(string(options))
	if err != nil {
		t.Fatal(err)
	}
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{
		UserHandle: mustUserIDBytes(userID),
	})
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	response := virtualwebauthn.CreateAttestationResponse(rp, authenticator, credential, *parsedOptions)
	registered, err := adapter.FinishRegistration(user, registrationSession, []byte(response))
	if err != nil {
		t.Fatalf("FinishRegistration() error = %v", err)
	}
	if registered.Credential.UserID != userID || registered.Credential.CredentialID == "" || registered.Credential.PublicKey == "" {
		t.Fatalf("registered credential = %+v", registered.Credential)
	}
	authenticator.AddCredential(credential)
	user.Credentials = append(user.Credentials, registered.Credential)

	assertionOptions, authenticationSession, err := adapter.BeginAuthentication(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatal(err)
	}
	parsedAssertion, err := virtualwebauthn.ParseAssertionOptions(string(assertionOptions))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsedAssertion.AllowCredentials) != 0 {
		t.Fatalf("discoverable login unexpectedly constrained credentials: %v", parsedAssertion.AllowCredentials)
	}
	credential.Counter++
	assertion := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credential, *parsedAssertion)
	result, err := adapter.FinishAuthentication(authenticationSession, []byte(assertion), func(rawID, handle []byte) (User, error) {
		if !bytes.Equal(rawID, credential.ID) || !bytes.Equal(handle, mustUserIDBytes(userID)) {
			t.Fatalf("resolver input rawID=%x handle=%x", rawID, handle)
		}
		return user, nil
	})
	if err != nil {
		t.Fatalf("FinishAuthentication() error = %v", err)
	}
	if result.UserID != userID || result.Credential.SignCount != 1 {
		t.Fatalf("authentication result = %+v", result)
	}
}

func FuzzWebAuthnResponseBoundary(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"id":"AA","rawId":"AA","type":"public-key","response":{}}`),
		[]byte(`{"id":"<script>","type":"public-key"}`),
		{0xff, 0xfe},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = protocol.ParseCredentialCreationResponseBytes(data)
		_, _ = protocol.ParseCredentialRequestResponseBytes(data)
	})
}

func TestWebAuthnAdapterRejectsInvalidBoundaryValues(t *testing.T) {
	adapter, err := NewGoWebAuthn("drive.example.test", "EndlessFS", "https://drive.example.test")
	if err != nil {
		t.Fatal(err)
	}
	displayName, _ := domain.ParseDisplayName("Boundary User")
	validUser := User{ID: testUserID(t, 0x39), DisplayName: displayName}
	invalidUsers := []User{{}, {ID: validUser.ID}}
	for _, user := range invalidUsers {
		if _, _, err := adapter.BeginRegistration(user, bytes.Repeat([]byte{1}, 32)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("BeginRegistration invalid user = %v", err)
		}
		if _, err := adapter.FinishRegistration(user, []byte(`{}`), []byte(`{}`)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("FinishRegistration invalid user = %v", err)
		}
	}
	if _, _, err := adapter.BeginRegistration(validUser, bytes.Repeat([]byte{1}, 31)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("short registration challenge = %v", err)
	}
	if _, _, err := adapter.BeginAuthentication(bytes.Repeat([]byte{1}, 31)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("short authentication challenge = %v", err)
	}
	if _, err := adapter.FinishRegistration(validUser, []byte(`not-json`), []byte(`{}`)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid registration state = %v", err)
	}
	if _, err := adapter.FinishAuthentication([]byte(`not-json`), []byte(`{}`), func(_, _ []byte) (User, error) { return validUser, nil }); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid authentication state = %v", err)
	}
	if _, _, err := encodeOpaque(make(chan int), struct{}{}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("opaque options encoding = %v", err)
	}
	if _, _, err := encodeOpaque(struct{}{}, make(chan int)); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("opaque session encoding = %v", err)
	}

	if _, err := newWebAuthnUser(User{ID: validUser.ID, DisplayName: displayName, Credentials: []model.Credential{{CredentialID: "%%%", PublicKey: "AA"}}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid stored credential ID = %v", err)
	}
	if _, err := libraryCredential(model.Credential{CredentialID: "AA", PublicKey: "%%%"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid stored public key = %v", err)
	}
	backup := true
	stored := model.Credential{CredentialID: "AA", PublicKey: "AQ", Transports: []string{"internal"}, BackupEligible: &backup, BackupState: &backup, SignCount: 7}
	library, err := libraryCredential(stored)
	if err != nil || !library.Flags.BackupEligible || !library.Flags.BackupState || library.Authenticator.SignCount != 7 {
		t.Fatalf("library credential = %+v, %v", library, err)
	}
	if _, err := modelCredential(validUser.ID, nil, domain.CredentialLabel{}, false); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil verified credential = %v", err)
	}
	if _, err := modelCredential(validUser.ID, &wa.Credential{ID: []byte{1}}, domain.CredentialLabel{}, false); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("incomplete verified credential = %v", err)
	}
	label, _ := domain.ParseCredentialLabel("Laptop")
	record, err := modelCredential(validUser.ID, &wa.Credential{ID: []byte{1}, PublicKey: []byte{2}, Transport: []protocol.AuthenticatorTransport{protocol.Internal}}, label, true)
	if err != nil || record.Label == nil || record.Label.String() != "Laptop" || len(record.Transports) != 1 {
		t.Fatalf("verified credential conversion = %+v, %v", record, err)
	}
}

func TestIntegrationGoWebAuthnRejectsWrongOriginRPChallengeAndVerification(t *testing.T) {
	rp := virtualwebauthn.RelyingParty{Name: "EndlessFS", ID: "drive.example.test", Origin: "https://drive.example.test"}
	adapter, err := NewGoWebAuthn(rp.ID, rp.Name, rp.Origin)
	if err != nil {
		t.Fatal(err)
	}
	userID := testUserID(t, 0x61)
	displayName, _ := domain.ParseDisplayName("Security Test")
	user := User{ID: userID, DisplayName: displayName}

	tests := []struct {
		name           string
		mutateRP       func(*virtualwebauthn.RelyingParty)
		mutateAuth     func(*virtualwebauthn.Authenticator)
		mutateOpts     func(*virtualwebauthn.AttestationOptions)
		mutateResponse func(string) string
	}{
		{name: "wrong origin", mutateRP: func(value *virtualwebauthn.RelyingParty) { value.Origin = "https://evil.example" }},
		{name: "wrong RP ID", mutateRP: func(value *virtualwebauthn.RelyingParty) { value.ID = "evil.example" }},
		{name: "wrong challenge", mutateOpts: func(value *virtualwebauthn.AttestationOptions) { value.Challenge[0] ^= 0xff }},
		{name: "user not verified", mutateAuth: func(value *virtualwebauthn.Authenticator) { value.Options.UserNotVerified = true }},
		{name: "invalid signature", mutateResponse: tamperWebAuthnSignature},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, session, err := adapter.BeginRegistration(user, bytes.Repeat([]byte{0x72}, 32))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := virtualwebauthn.ParseAttestationOptions(string(options))
			if err != nil {
				t.Fatal(err)
			}
			attemptRP := rp
			authenticator := virtualwebauthn.NewAuthenticator()
			if test.mutateRP != nil {
				test.mutateRP(&attemptRP)
			}
			if test.mutateAuth != nil {
				test.mutateAuth(&authenticator)
			}
			if test.mutateOpts != nil {
				test.mutateOpts(parsed)
			}
			credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
			response := virtualwebauthn.CreateAttestationResponse(attemptRP, authenticator, credential, *parsed)
			if test.mutateResponse != nil {
				response = test.mutateResponse(response)
			}
			if _, err := adapter.FinishRegistration(user, session, []byte(response)); err == nil {
				t.Fatal("invalid WebAuthn response was accepted")
			}
		})
	}
}

func TestIntegrationGoWebAuthnAuthenticationNegativeMatrix(t *testing.T) {
	rp := virtualwebauthn.RelyingParty{Name: "EndlessFS", ID: "drive.example.test", Origin: "https://drive.example.test"}
	adapter, err := NewGoWebAuthn(rp.ID, rp.Name, rp.Origin)
	if err != nil {
		t.Fatal(err)
	}
	userID := testUserID(t, 0x41)
	displayName, _ := domain.ParseDisplayName("Login Security")
	user := User{ID: userID, DisplayName: displayName}
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: mustUserIDBytes(userID)})
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	registrationOptions, registrationSession, _ := adapter.BeginRegistration(user, bytes.Repeat([]byte{0x42}, 32))
	parsedRegistration, _ := virtualwebauthn.ParseAttestationOptions(string(registrationOptions))
	registrationResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, credential, *parsedRegistration)
	registered, err := adapter.FinishRegistration(user, registrationSession, []byte(registrationResponse))
	if err != nil {
		t.Fatal(err)
	}
	user.Credentials = []model.Credential{registered.Credential}
	authenticator.AddCredential(credential)

	tests := []struct {
		name           string
		mutateRP       func(*virtualwebauthn.RelyingParty)
		mutateAuth     func(*virtualwebauthn.Authenticator)
		mutateOptions  func(*virtualwebauthn.AssertionOptions)
		mutateResponse func(string) string
		resolve        UserResolver
	}{
		{name: "wrong origin", mutateRP: func(value *virtualwebauthn.RelyingParty) { value.Origin = "https://evil.example" }},
		{name: "wrong RP ID", mutateRP: func(value *virtualwebauthn.RelyingParty) { value.ID = "evil.example" }},
		{name: "wrong challenge", mutateOptions: func(value *virtualwebauthn.AssertionOptions) { value.Challenge[0] ^= 0xff }},
		{name: "wrong user handle", mutateAuth: func(value *virtualwebauthn.Authenticator) { value.Options.UserHandle = bytes.Repeat([]byte{0x99}, 16) }},
		{name: "user not verified", mutateAuth: func(value *virtualwebauthn.Authenticator) { value.Options.UserNotVerified = true }},
		{name: "invalid signature", mutateResponse: tamperWebAuthnSignature},
		{name: "wrong owner", resolve: func(_, _ []byte) (User, error) {
			other := user
			other.ID = testUserID(t, 0x88)
			return other, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, session, err := adapter.BeginAuthentication(bytes.Repeat([]byte{0x43}, 32))
			if err != nil {
				t.Fatal(err)
			}
			parsed, _ := virtualwebauthn.ParseAssertionOptions(string(options))
			attemptRP := rp
			attemptAuthenticator := authenticator
			if test.mutateRP != nil {
				test.mutateRP(&attemptRP)
			}
			if test.mutateAuth != nil {
				test.mutateAuth(&attemptAuthenticator)
			}
			if test.mutateOptions != nil {
				test.mutateOptions(parsed)
			}
			response := virtualwebauthn.CreateAssertionResponse(attemptRP, attemptAuthenticator, credential, *parsed)
			if test.mutateResponse != nil {
				response = test.mutateResponse(response)
			}
			resolver := test.resolve
			if resolver == nil {
				resolver = func(rawID, handle []byte) (User, error) {
					if !bytes.Equal(rawID, credential.ID) || !bytes.Equal(handle, mustUserIDBytes(userID)) {
						return User{}, domain.NewError(domain.ErrorUnauthenticated, "owner mismatch")
					}
					return user, nil
				}
			}
			if _, err := adapter.FinishAuthentication(session, []byte(response), resolver); err == nil {
				t.Fatal("invalid assertion was accepted")
			}
		})
	}
}

func tamperWebAuthnSignature(response string) string {
	var value map[string]any
	if json.Unmarshal([]byte(response), &value) != nil {
		return response + "x"
	}
	nested, _ := value["response"].(map[string]any)
	for _, field := range []string{"signature", "attestationObject"} {
		if encoded, ok := nested[field].(string); ok && len(encoded) > 2 {
			position := len(encoded) / 2
			replacement := byte('A')
			if encoded[position] == 'A' {
				replacement = 'B'
			}
			nested[field] = encoded[:position] + string(replacement) + encoded[position+1:]
			break
		}
	}
	modified, _ := json.Marshal(value)
	return string(modified)
}

func assertRegistrationPolicy(t *testing.T, options json.RawMessage, challenge []byte, userID domain.UserID) {
	t.Helper()
	var payload struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
			AuthenticatorSelection struct {
				ResidentKey      string `json:"residentKey"`
				UserVerification string `json:"userVerification"`
			} `json:"authenticatorSelection"`
			Attestation string `json:"attestation"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &payload); err != nil {
		t.Fatal(err)
	}
	wantChallenge := base64.RawURLEncoding.EncodeToString(challenge)
	if payload.PublicKey.Challenge != wantChallenge || payload.PublicKey.User.Name != userID.String() {
		t.Fatalf("registration identity/challenge = %+v", payload.PublicKey)
	}
	if payload.PublicKey.AuthenticatorSelection.ResidentKey != "required" || payload.PublicKey.AuthenticatorSelection.UserVerification != "required" || payload.PublicKey.Attestation != "none" {
		t.Fatalf("registration policy = %+v", payload.PublicKey)
	}
}

func testUserID(t *testing.T, fill byte) domain.UserID {
	t.Helper()
	value, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
