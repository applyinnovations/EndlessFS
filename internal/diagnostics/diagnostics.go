// Package diagnostics owns the separately authenticated operator-only surface.
package diagnostics

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

const maximumProfileBytes = 8 << 20

var errProfileLimit = errors.New("aggregate profile exceeds output limit")

// Handler deliberately does not register net/http/pprof's default mux, raw
// heap dumps, goroutine stacks, runtime execution traces, cmdline, or debug text.
func Handler(observer *telemetry.Observer, token string) http.Handler {
	return newHandler(observer, token, captureProfile)
}

type profileCapture func(context.Context, string, io.Writer, time.Duration) error

func newHandler(observer *telemetry.Observer, token string, capture profileCapture) http.Handler {
	profileSlot := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		credentials := r.Header.Values("Authorization")
		if len(token) < 32 || len(credentials) != 1 || !strings.HasPrefix(credentials[0], "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(credentials[0], "Bearer ")), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "diagnostics authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/metrics" {
			if r.URL.RawQuery != "" {
				http.Error(w, "unexpected query", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			observer.WritePrometheus(w)
			return
		}
		cpu := r.URL.Path == "/debug/pprof/profile"
		name := ""
		switch r.URL.Path {
		case "/debug/pprof/heap":
			name = "heap"
		case "/debug/pprof/allocs":
			name = "allocs"
		case "/debug/pprof/profile":
			name = "cpu"
		default:
			http.NotFound(w, r)
			return
		}
		seconds := 10
		if cpu {
			parsed, err := url.ParseQuery(r.URL.RawQuery)
			query := parsed["seconds"]
			if r.URL.RawQuery != "" {
				if err != nil || len(parsed) != 1 || len(query) != 1 {
					http.Error(w, "invalid profile interval", 400)
					return
				}
				seconds, err = strconv.Atoi(query[0])
			}
			if err != nil || seconds < 1 || seconds > 10 || r.Context().Err() != nil {
				http.Error(w, "invalid profile interval", 400)
				return
			}
		} else if r.URL.RawQuery != "" {
			http.Error(w, "unexpected query", 400)
			return
		}
		select {
		case profileSlot <- struct{}{}:
			defer func() { <-profileSlot }()
		default:
			http.Error(w, "profile capture busy", http.StatusTooManyRequests)
			return
		}
		buffer := &profileBuffer{}
		err := capture(r.Context(), name, buffer, time.Duration(seconds)*time.Second)
		if err != nil || buffer.overflow {
			http.Error(w, "profile capture unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(buffer.Bytes())
	})
}
func captureProfile(ctx context.Context, name string, output io.Writer, duration time.Duration) error {
	if name == "cpu" {
		return captureCPU(ctx, output, duration)
	}
	return pprof.Lookup(name).WriteTo(output, 0)
}
func captureCPU(ctx context.Context, output io.Writer, duration time.Duration) error {
	if err := pprof.StartCPUProfile(output); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	pprof.StopCPUProfile()
	return nil
}

type profileBuffer struct {
	bytes.Buffer
	overflow bool
}

func (buffer *profileBuffer) Write(body []byte) (int, error) {
	if buffer.Len()+len(body) > maximumProfileBytes {
		buffer.overflow = true
		return 0, errProfileLimit
	}
	return buffer.Buffer.Write(body)
}

var _ io.Writer = (*profileBuffer)(nil)
