package memory_test

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/provider/providercontract"
)

func TestContractMemoryProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("provider contract suite")
	}
	providercontract.Run(t, func(t *testing.T) providercontract.Harness {
		clock := domain.NewFixedClock(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
		provider := providermemory.New(providermemory.Options{
			Clock:       clock,
			IDs:         domain.NewIDGenerator(bytes.NewReader(deterministicBytes(1 << 20))),
			UploadTTL:   5 * time.Minute,
			DownloadTTL: time.Minute,
		})
		server := httptest.NewServer(provider)
		t.Cleanup(server.Close)
		if err := provider.SetDataPlaneBaseURL(server.URL); err != nil {
			t.Fatal(err)
		}
		return providercontract.Harness{
			Storage: provider,
			Client:  server.Client(),
			Advance: clock.Advance,
			InjectFault: func(operation, fault string) {
				provider.InjectFault(operation, providermemory.Fault(fault))
			},
			UploadOffset:   provider.UploadOffset,
			SimulateOffset: provider.SimulateUploadOffset,
			ByteCounts: func() providercontract.ByteCounts {
				metrics := provider.Instrumentation()
				return providercontract.ByteCounts{
					Control:  metrics.ControlPlaneBytes,
					Upload:   metrics.UploadBytes,
					Download: metrics.DownloadBytes,
				}
			},
		}
	})
}

func deterministicBytes(size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = byte(index*31 + 17)
	}
	return value
}
