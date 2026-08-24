package portable_test

// Historical migration tests must start from immutable bytes written by the
// recorded predecessor binary. These helpers inspect or corrupt those bound
// fixtures; they never attempt to synthesize an old epoch with current code.

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func schemaMigrationOptions(backend objectstore.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) portable.Options {
	return portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32), Scheduler: scheduler,
	}
}

func schemaSplitMigrationOptions(stateBackend, fileBackend objectstore.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) portable.Options {
	options := schemaMigrationOptions(stateBackend, clock, seed, scheduler)
	options.FileBackend = fileBackend
	return options
}

func assertRecursiveFeatureInactive(t *testing.T, objects map[string][]byte) {
	t.Helper()
	var superblock storageformat.Superblock
	if err := state.DecodeJSONWithLimit(objects[storageformat.SuperblockKey().String()], &superblock, storageformat.MaxCanonicalBytes); err != nil {
		t.Fatal(err)
	}
	if containsFeature(superblock.RequiredFeatures, storageformat.FeatureRecursiveBytes) {
		t.Fatalf("recursive-byte feature activated after failed migration: %v", superblock.RequiredFeatures)
	}
	if containsFeature(superblock.RequiredFeatures, storageformat.FeatureRecursiveFileCounts) {
		t.Fatalf("recursive-file-count feature activated after failed migration: %v", superblock.RequiredFeatures)
	}
}

func containsFeature(features []string, wanted string) bool {
	for _, feature := range features {
		if feature == wanted {
			return true
		}
	}
	return false
}

func mutateSchemaFixturePage(t *testing.T, objects map[string][]byte, mutate func(*storageformat.DirectoryPage) bool) {
	t.Helper()
	mutated := false
	for key, body := range objects {
		if !strings.Contains(key, "/pages/") || !strings.HasSuffix(key, ".json") {
			continue
		}
		parsed := storageformatKey(t, key)
		var envelope storageformat.Envelope
		var page storageformat.DirectoryPage
		if err := storageformat.DecodeEnvelope(body, parsed, "directory-page-v1", &envelope, &page); err != nil {
			t.Fatal(err)
		}
		if mutate(&page) {
			objects[key] = mustEnvelope(t, "directory-page-v1", parsed, envelope.Revision, page)
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("legacy fixture has no matching page")
	}
}

func newPortableDataServer(t *testing.T, backend *objectmemory.Backend, clock *domain.FixedClock, seed byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	return server
}

func storageformatKey(t *testing.T, value string) objectstore.Key {
	t.Helper()
	key, err := objectstore.ParseKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustCanonical(t *testing.T, value any) []byte {
	t.Helper()
	body, err := storageformat.EncodeCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustEnvelope(t *testing.T, schema string, key objectstore.Key, revision uint64, value any) []byte {
	t.Helper()
	body, err := storageformat.EncodeEnvelope(schema, key, revision, value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func entryLogicalVersion(t *testing.T, entry storageformat.DirectoryEntry) string {
	t.Helper()
	entry.LogicalVersion = ""
	return storageformat.Digest(mustCanonical(t, entry))
}
