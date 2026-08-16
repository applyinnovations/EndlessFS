// Package secret centralizes bearer-token hashing, validation, and redaction.
package secret

import (
	"crypto/hmac"
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

func KeyedHash(key Value, value string) string {
	mac := hmac.New(sha256.New, []byte(key.Reveal()))
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func MatchesKeyedHash(key Value, value, encodedHash string) bool {
	expected, err := base64.RawURLEncoding.DecodeString(encodedHash)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key.Reveal()))
	_, _ = mac.Write([]byte(value))
	return hmac.Equal(mac.Sum(nil), expected)
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
