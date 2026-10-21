package e2e

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const e2eParallelismEnvironment = "ENDLESSFS_E2E_PARALLELISM"

type e2eFixturePool struct {
	slots   chan struct{}
	active  atomic.Int32
	maximum atomic.Int32
}

func newE2EFixturePool(limit int) *e2eFixturePool {
	return &e2eFixturePool{slots: make(chan struct{}, limit)}
}

func (pool *e2eFixturePool) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case pool.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-pool.slots
			return err
		}
		active := pool.active.Add(1)
		for maximum := pool.maximum.Load(); active > maximum; maximum = pool.maximum.Load() {
			if pool.maximum.CompareAndSwap(maximum, active) {
				break
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (pool *e2eFixturePool) release() {
	pool.active.Add(-1)
	<-pool.slots
}

type e2eSchedule struct {
	limit        int
	forcedSerial bool
}

func resolveE2ESchedule(environment []string, profiling bool) (e2eSchedule, error) {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			return e2eSchedule{}, errors.New("invalid E2E scheduling environment")
		}
		values[name] = value
	}
	limit := 2 // Conservative candidate default; selection still needs measurement.
	switch values[e2eParallelismEnvironment] {
	case "":
	case "1":
		limit = 1
	case "2":
		limit = 2
	case "4":
		limit = 4
	default:
		return e2eSchedule{}, errors.New("E2E parallelism must be exactly1,2 or4")
	}
	serial := profiling
	for name, value := range values {
		if value == "" || !strings.HasPrefix(name, "ENDLESSFS_") {
			continue
		}
		// This is an output-only copy of the ordinary atomic coverage file.
		// Its destination does not change the workload or scheduling mode.
		if name == "ENDLESSFS_COVERAGE_PROFILE" {
			continue
		}
		if strings.Contains(name, "SCREENSHOT") || strings.Contains(name, "EVIDENCE") ||
			strings.Contains(name, "QUALIFICATION") || strings.Contains(name, "BENCHMARK") ||
			strings.Contains(name, "_PROFILE") {
			serial = true
		}
	}
	if serial {
		limit = 1
	}
	return e2eSchedule{limit: limit, forcedSerial: serial}, nil
}

var e2eScheduleOnce sync.Once
var e2eScheduleConfig e2eSchedule
var e2eScheduleError error
var e2eSharedFixturePool *e2eFixturePool

// parallelE2EFixture is called FIRST by each top-level TestE2E function.
// It is not called by factories, shared helpers or subtests. All fixtures,
// contexts, browser launches and fixture cleanup registrations follow it.
func parallelE2EFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		// Preserve the original body/helper skip or fatal contract verbatim.
		// In particular, hardware selection without RUN_E2E must still fail.
		return
	}
	if !strings.HasPrefix(t.Name(), "TestE2E") || strings.Contains(t.Name(), "/") {
		t.Fatal("E2E fixture scheduling is top-level only")
	}
	e2eScheduleOnce.Do(func() {
		profiling := false
		// The normal atomic coverage profile is deliberately NOT a serial
		// selector. Optional CPU/alloc/block/mutex/trace instrumentation is.
		for _, name := range []string{"test.cpuprofile", "test.memprofile", "test.blockprofile", "test.mutexprofile", "test.trace"} {
			if value := flag.Lookup(name); value != nil && value.Value.String() != "" {
				profiling = true
			}
		}
		e2eScheduleConfig, e2eScheduleError = resolveE2ESchedule(os.Environ(), profiling)
		if e2eScheduleError == nil {
			e2eSharedFixturePool = newE2EFixturePool(e2eScheduleConfig.limit)
		}
	})
	if e2eScheduleError != nil {
		t.Fatal(e2eScheduleError)
	}
	held := false
	var acquired time.Time
	// Register FIRST: testing.Cleanup is LIFO, so this releases LAST, after
	// body defers and every later browser, server, ledger and TempDir cleanup.
	// t.Context is canceled before cleanup; release deliberately needs no context.
	t.Cleanup(func() {
		if held {
			e2eSharedFixturePool.release()
			t.Logf("e2e fixture lease released active=%d peak=%d fixture_seconds=%.6f",
				e2eSharedFixturePool.active.Load(), e2eSharedFixturePool.maximum.Load(), time.Since(acquired).Seconds())
		}
	})
	if e2eScheduleConfig.limit > 1 {
		t.Parallel()
	}
	wait := time.Now()
	if err := e2eSharedFixturePool.acquire(t.Context()); err != nil {
		t.Fatal("E2E fixture lease canceled")
	}
	held = true
	acquired = time.Now()
	t.Logf("e2e fixture lease acquired limit=%d active=%d forced_serial=%t queue_seconds=%.6f",
		e2eScheduleConfig.limit, e2eSharedFixturePool.active.Load(), e2eScheduleConfig.forcedSerial, time.Since(wait).Seconds())
}
