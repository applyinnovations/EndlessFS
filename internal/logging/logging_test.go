package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONLoggerRedactsSensitiveFieldsAtEveryLevel(t *testing.T) {
	t.Parallel()
	const marker = "sensitive-marker-that-must-not-appear"
	var output bytes.Buffer
	logger := NewJSON(&output, slog.LevelDebug)
	logger.DebugContext(context.Background(), "adversarial_event",
		"sessionCookie", marker,
		"csrf_token", marker,
		"challenge", marker,
		"credentialID", marker,
		"inviteToken", marker,
		"shareURL", "https://drive.invalid/public/shares/"+marker+"?capability="+marker,
		"recovery", marker,
		"bootstrap", marker,
		"authorization", "Bearer "+marker,
		"providerObjectKey", marker,
		"body", marker,
		"virtualPath", "/private/"+marker,
	)
	got := output.String()
	if strings.Contains(got, marker) {
		t.Fatalf("structured log leaked sensitive marker: %s", got)
	}
	if count := strings.Count(got, Redacted); count < 12 {
		t.Fatalf("structured log redactions = %d, want at least 12: %s", count, got)
	}
	for _, required := range []string{`"level":"DEBUG"`, `"msg":"adversarial_event"`} {
		if !strings.Contains(got, required) {
			t.Fatalf("structured log missing %s: %s", required, got)
		}
	}
}

func TestJSONLoggerHonorsConfiguredLevelAndKeepsSafeFields(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := NewJSON(&output, slog.LevelWarn)
	logger.Info("not_written", "requestID", "request-123")
	logger.Warn("request_complete", "requestID", "request-123", "route", "/api/v1/files/{operation}", "result", "success")
	got := output.String()
	if strings.Contains(got, "not_written") || !strings.Contains(got, "request-123") || !strings.Contains(got, "/api/v1/files/{operation}") {
		t.Fatalf("level filtering or safe fields = %s", got)
	}
}

func FuzzStructuredLogRedaction(f *testing.F) {
	for _, seed := range []string{"token", "line\r\nbreak", "https://drive.invalid/capability?q=secret", string([]byte{0xff})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		const marker = "fixed-sensitive-marker"
		var output bytes.Buffer
		NewJSON(&output, slog.LevelDebug).Debug("fuzz", "authorization", value+marker, "virtualPath", "/"+value+marker)
		if strings.Contains(output.String(), marker) {
			t.Fatalf("sensitive log attribute was not redacted")
		}
	})
}
