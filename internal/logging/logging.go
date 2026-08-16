// Package logging constructs structured loggers that redact security-boundary fields.
package logging

import (
	"io"
	"log/slog"
	"strings"
	"unicode"
)

const Redacted = "[REDACTED]"

// NewJSON returns a JSON logger whose minimum level is fixed at construction
// and whose handler redacts sensitive attributes at every log level.
func NewJSON(output io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attribute slog.Attr) slog.Attr {
			if sensitiveKey(attribute.Key) {
				return slog.String(attribute.Key, Redacted)
			}
			return attribute
		},
	})
	return slog.New(handler)
}

func sensitiveKey(key string) bool {
	normalized := strings.Map(func(value rune) rune {
		if unicode.IsLetter(value) || unicode.IsDigit(value) {
			return unicode.ToLower(value)
		}
		return -1
	}, key)
	for _, fragment := range []string{
		"authorization", "bootstrap", "body", "capability", "challenge",
		"cookie", "credential", "csrf", "invite", "objectkey", "path",
		"preview", "providerkey", "query", "recovery", "secret", "share",
		"token", "url",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}
