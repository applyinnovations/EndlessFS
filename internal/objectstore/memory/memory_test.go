package memory_test

import (
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/objectstore/objectstorecontract"
)

func TestContractMemoryObjectBackend(t *testing.T) {
	objectstorecontract.Run(t, func(*testing.T) objectstore.Backend {
		return memory.New()
	})
}
