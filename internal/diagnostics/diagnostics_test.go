package diagnostics

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsRequiresHeaderAuthenticationAndExcludesUnsafeProfiles(t *testing.T) {
	token := strings.Repeat("k", 43)
	handler := Handler(telemetry.New(nil, nil), token)
	for _, test := range []struct {
		path, credential string
		want             int
	}{
		{"/metrics", "", 401}, {"/metrics", "Bearer wrong", 401},
		{"/metrics?token=" + token, "", 401}, {"/metrics", "Bearer " + token, 200},
		{"/debug/pprof/heap", "Bearer " + token, 200},
		{"/debug/pprof/allocs", "Bearer " + token, 200},
		{"/debug/pprof/heap?debug=1", "Bearer " + token, 400},
		{"/debug/pprof/profile?seconds=100", "Bearer " + token, 400},
		{"/debug/pprof/profile?seconds=1&invalid=%GG", "Bearer " + token, 400},
		{"/debug/pprof/cmdline", "Bearer " + token, 404},
		{"/debug/pprof/goroutine", "Bearer " + token, 404},
		{"/debug/pprof/trace", "Bearer " + token, 404},
		{"/debug/pprof/", "Bearer " + token, 404},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		if test.credential != "" {
			request.Header.Set("Authorization", test.credential)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("%s status=%d want %d", test.path, response.Code, test.want)
		}
		if bytes.Contains(response.Body.Bytes(), []byte(token)) {
			t.Fatal("diagnostic secret reflected")
		}
	}
}
func TestAggregateCPUProfileCancellationAndOutputBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var profile profileBuffer
	if err := captureCPU(ctx, &profile, time.Second); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(profile.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || len(body) == 0 {
		t.Fatal("CPU profile was not a valid aggregate pprof stream")
	}
	if bytes.Contains(body, []byte("sentinel-private-user-buffer")) {
		t.Fatal("private data entered profile")
	}
	var bounded profileBuffer
	if _, err := bounded.Write(make([]byte, maximumProfileBytes+1)); !errors.Is(err, errProfileLimit) || !bounded.overflow || bounded.Len() != 0 {
		t.Fatal("profile buffer exceeded its bound")
	}
}

func TestEmptyTokenCannotEnableDiagnostics(t *testing.T) {
	handler := Handler(telemetry.New(nil, nil), "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code == 200 {
		t.Fatal("empty token exposed metrics")
	}
}
func TestProfileCaptureIsSerializedAndErrorsAreSafe(t *testing.T) {
	token := strings.Repeat("k", 43)
	entered, release := make(chan struct{}), make(chan struct{})
	handler := newHandler(telemetry.New(nil, nil), token, func(context.Context, string, io.Writer, time.Duration) error {
		close(entered)
		<-release
		return errors.New("sentinel-secret-file")
	})
	request := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	first := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(first, request(http.MethodGet, "/debug/pprof/profile?seconds=1"))
		close(finished)
	}()
	<-entered
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request(http.MethodGet, "/debug/pprof/heap"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatal("overlapping profile capture admitted")
	}
	close(release)
	<-finished
	if first.Code != http.StatusServiceUnavailable || strings.Contains(first.Body.String(), "sentinel") {
		t.Fatal("capture failure leaked error or was unreported")
	}
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/metrics", 405}, {http.MethodGet, "/metrics?debug=1", 400}, {http.MethodGet, "/debug/pprof/profile?seconds=1&seconds=2", 400}, {http.MethodGet, "/debug/pprof/profile?seconds=bad", 400},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request(test.method, test.path))
		if response.Code != test.status {
			t.Errorf("%s %s: %d", test.method, test.path, response.Code)
		}
	}
}
