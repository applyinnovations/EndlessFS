package telemetry

import (
	"bytes"
	"context"
	"errors"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"strings"
	"sync"
	"testing"
)

func TestOperationalMetricsPreserveResultBytesAndConcurrentAccounting(t *testing.T) {
	observer := New(nil, nil)
	ctx := Context(context.Background(), observer)
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			_, activity := Start(ctx, ProviderGet, State)
			activity.Bytes(123, 0)
			activity.End(domain.ErrUnauthorized)
			activity.End(nil) // An ended operation cannot be counted twice.
		})
	}
	workers.Wait()
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	for _, want := range []string{
		`endlessfs_operations_total{operation="provider.get",role="state",result="denied"} 64`,
		`endlessfs_operation_bytes_total{operation="provider.get",role="state",direction="read"} 7872`,
		`endlessfs_operations_active{operation="provider.get",role="state"} 0`,
		`endlessfs_operation_duration_seconds_count{operation="provider.get",role="state",result="denied"} 64`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing signal %s", want)
		}
	}
}

func TestTelemetryRejectsUnknownDimensionsAndNeverExportsRawErrors(t *testing.T) {
	observer := New(nil, nil)
	ctx := Context(context.Background(), observer)
	for range 1000 {
		_, activity := Start(ctx, Operation(255), Role(255))
		activity.Route("GET /sentinel-secret-file?token=sentinel-capability")
		activity.End(errors.New("sentinel-provider-key/sentinel-user-file"))
	}
	_, activity := Start(ctx, HTTP, Application)
	activity.Route("GET /api/v1/public/shares/{token}")
	activity.End(domain.ErrNotFound)
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if strings.Contains(output.String(), "sentinel") {
		t.Fatal("unsafe data entered metrics")
	}
	if !strings.Contains(output.String(), `route="GET /api/v1/public/shares/{token}"`) {
		t.Fatal("closed route template is missing")
	}
}

func TestDisabledTelemetryDoesNotChangeContextOrOperation(t *testing.T) {
	ctx := context.Background()
	next, activity := Start(ctx, DomainCommit, State)
	if next != ctx || activity != nil {
		t.Fatal("disabled collection changed context or allocated activity")
	}
	activity.End(domain.ErrConflict)
	activity.Bytes(1, 2)
}
