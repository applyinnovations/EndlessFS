package secret

import (
	"bytes"
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
