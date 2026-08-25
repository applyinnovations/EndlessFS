package secret

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestTokenHashUsesConstantShapeAndMatches(t *testing.T) {
	t.Parallel()

	token, err := domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x44}, 32))).BearerToken()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidBearerToken(token) {
		t.Fatal("generated token was not accepted")
	}
	hash := Hash(token)
	if !MatchesHash(token, hash) || MatchesHash(token+"x", hash) || MatchesHash(token, "invalid") {
		t.Fatal("unexpected token/hash match behavior")
	}
}

func TestKeyedHashBindsProtectionKeyAndValue(t *testing.T) {
	key := Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32)))
	otherKey := Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	encoded := KeyedHash(key, "browser binding")
	if !MatchesKeyedHash(key, "browser binding", encoded) {
		t.Fatal("keyed hash did not match")
	}
	if MatchesKeyedHash(otherKey, "browser binding", encoded) || MatchesKeyedHash(key, "other", encoded) {
		t.Fatal("keyed hash did not bind key and value")
	}
}

func TestScopedBearerTokenRoundTripAndTamperDenial(t *testing.T) {
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	raw := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, 32))
	token, err := ScopeBearerToken(owner, raw)
	if err != nil {
		t.Fatal(err)
	}
	parsedOwner, secretPart, err := ParseScopedBearerToken(token)
	if err != nil || parsedOwner != owner || secretPart.Reveal() != raw || !ValidScopedBearerToken(token) {
		t.Fatalf("parsed token = %v %v %v", parsedOwner, secretPart, err)
	}
	for _, invalid := range []string{"", raw, "s2." + owner.String() + "." + raw, "s1.invalid." + raw, "s1." + owner.String() + ".invalid", token + ".extra"} {
		if ValidScopedBearerToken(invalid) {
			t.Errorf("accepted invalid scoped token %q", invalid)
		}
	}
}

func TestSecretValueCannotLeakThroughStringOrStructuredLog(t *testing.T) {
	t.Parallel()

	value := Value("raw-secret-value")
	if value.String() != Redacted {
		t.Fatalf("String() = %q", value.String())
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logger.Info("test", "token", value)
	if strings.Contains(output.String(), value.Reveal()) || !strings.Contains(output.String(), Redacted) {
		t.Fatalf("log output = %s", output.String())
	}
}
