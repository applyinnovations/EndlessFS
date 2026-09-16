package memory_test

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/objectstore/objectstorecontract"
)

func TestContractMemoryObjectBackend(t *testing.T) {
	objectstorecontract.Run(t, func(*testing.T) objectstore.Backend {
		return memory.New()
	})
}

func TestContractMemoryUploadAbort(t *testing.T) {
	objectstorecontract.RunUploadAbort(t, func(t *testing.T) objectstorecontract.UploadAbortHarness {
		backend := memory.New()
		server := httptest.NewServer(backend)
		t.Cleanup(server.Close)
		clock := domain.NewFixedClock(time.Date(2072, 1, 2, 3, 4, 5, 0, time.UTC))
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x65}, 4096)))); err != nil {
			t.Fatal(err)
		}
		return objectstorecontract.UploadAbortHarness{Backend: backend, Client: server.Client(), Now: clock.Now()}
	})
}
