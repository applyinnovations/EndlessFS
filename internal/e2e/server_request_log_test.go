package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// serverRequestLog keeps the most recent requests the fixture servers handled,
// and the ones still in flight, so a failed wait shows what the server was
// doing.
type serverRequestLog struct {
	mu       sync.Mutex
	next     int
	recent   []string
	inFlight map[int]string
	started  time.Time
}

const serverRequestLogSize = 40

func newServerRequestLog(t *testing.T) *serverRequestLog {
	log := &serverRequestLog{inFlight: map[int]string{}, started: time.Now()}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		pending := make([]string, 0, len(log.inFlight))
		for _, request := range log.inFlight {
			pending = append(pending, request)
		}
		t.Logf("server requests in flight at failure: %s", strings.Join(pending, ", "))
		t.Logf("last server requests: %s", strings.Join(log.recent, ", "))
	})
	return log
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *statusRecorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *statusRecorder) Flush() {
	if flusher, ok := recorder.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (recorder *statusRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }

func (log *serverRequestLog) wrap(plane string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		label := fmt.Sprintf("%s %s %s @%.1fs", plane, request.Method, request.URL.Path, start.Sub(log.started).Seconds())
		log.mu.Lock()
		id := log.next
		log.next++
		log.inFlight[id] = label
		log.mu.Unlock()
		recorder := &statusRecorder{ResponseWriter: writer}
		defer func() {
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			log.mu.Lock()
			delete(log.inFlight, id)
			log.recent = append(log.recent, fmt.Sprintf("%s -> %d in %dms", label, status, time.Since(start).Milliseconds()))
			if len(log.recent) > serverRequestLogSize {
				log.recent = log.recent[len(log.recent)-serverRequestLogSize:]
			}
			log.mu.Unlock()
		}()
		next.ServeHTTP(recorder, request)
	})
}
