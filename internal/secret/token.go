// Package secret centralizes bearer-token hashing, validation, and redaction.
package secret

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
)

const Redacted = "[REDACTED]"

func ValidBearerToken(token string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == token
}

func Hash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func MatchesHash(token, encodedHash string) bool {
	digest := sha256.Sum256([]byte(token))
	expected, err := base64.RawURLEncoding.DecodeString(encodedHash)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(digest[:], expected) == 1
}

// Value can be carried internally while remaining safe if passed to slog.
type Value string

func (Value) LogValue() slog.Value {
	return slog.StringValue(Redacted)
}

func (Value) String() string {
	return Redacted
}

func (v Value) Reveal() string {
	return string(v)
}
