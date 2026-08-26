// Package secret centralizes bearer-token hashing, validation, and redaction.
package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const Redacted = "[REDACTED]"

func ValidBearerToken(token string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == token
}

// ScopeBearerToken embeds only the non-secret owner locator needed to select a
// bounded consistency domain. The complete token remains authenticated by the
// keyed hash stored in that domain; parsing the locator never authorizes it.
func ScopeBearerToken(owner domain.UserID, rawSecret string) (string, error) {
	if !owner.Valid() || !ValidBearerToken(rawSecret) {
		return "", domain.NewError(domain.ErrorInvalid, "invalid scoped bearer token material")
	}
	return "s1." + owner.String() + "." + rawSecret, nil
}

func ParseScopedBearerToken(token string) (domain.UserID, Value, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "s1" || !ValidBearerToken(parts[2]) {
		return domain.UserID{}, "", domain.NewError(domain.ErrorInvalid, "invalid scoped bearer token")
	}
	owner, err := domain.ParseUserID(parts[1])
	if err != nil {
		return domain.UserID{}, "", domain.NewError(domain.ErrorInvalid, "invalid scoped bearer token")
	}
	return owner, Value(parts[2]), nil
}

func ValidScopedBearerToken(token string) bool {
	_, _, err := ParseScopedBearerToken(token)
	return err == nil
}

// ScopeCapabilityToken adds an opaque logical-resource locator so a public
// capability can resolve one owner-local record without a list operation.
// Authorization still comes from matching the complete token's stored hash.
func ScopeCapabilityToken(owner domain.UserID, locator, rawSecret string) (string, error) {
	locatorBytes, err := base64.RawURLEncoding.DecodeString(locator)
	if !owner.Valid() || err != nil || len(locatorBytes) < 16 || base64.RawURLEncoding.EncodeToString(locatorBytes) != locator || !ValidBearerToken(rawSecret) {
		return "", domain.NewError(domain.ErrorInvalid, "invalid scoped capability token material")
	}
	return "c1." + owner.String() + "." + locator + "." + rawSecret, nil
}

func ParseScopedCapabilityToken(token string) (domain.UserID, string, Value, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != "c1" || !ValidBearerToken(parts[3]) {
		return domain.UserID{}, "", "", domain.NewError(domain.ErrorInvalid, "invalid scoped capability token")
	}
	owner, err := domain.ParseUserID(parts[1])
	locatorBytes, locatorErr := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || locatorErr != nil || len(locatorBytes) < 16 || base64.RawURLEncoding.EncodeToString(locatorBytes) != parts[2] {
		return domain.UserID{}, "", "", domain.NewError(domain.ErrorInvalid, "invalid scoped capability token")
	}
	return owner, parts[2], Value(parts[3]), nil
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
