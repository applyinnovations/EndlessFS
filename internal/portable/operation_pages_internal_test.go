package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestFileOperationStoresBoundedStepPagesWithoutEmbeddedPrerequisiteBodies(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2048, 3, 4, 5, 6, 7, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("bounded-operation-pages-0123456789", 1<<16)))
	user, _ := domain.ParseUserID("ampqampqampqampqampqag")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	trail, err := engine.Files().resolveDirectoryTrail(context.Background(), scope, domain.MustParseUserPath("/"))
	if err != nil {
		t.Fatal(err)
	}
	updates := make(map[string]directoryUpdate)
	entry := withCurrentTestFingerprint(storageformat.DirectoryEntry{
		Name: "bounded.bin", NameDigest: storageformat.NameDigest("bounded.bin"), Kind: domain.EntryFile,
		BlobID: "bounded-blob", Size: 1, MediaType: "application/octet-stream", ModifiedAt: clock.Now(),
	})
	if err := applyDirectoryEntryChange(updates, trail, nil, &entry); err != nil {
		t.Fatal(err)
	}
	prerequisites := make([]storageformat.MutationObject, 900)
	for index := range prerequisites {
		prerequisites[index] = storageformat.MutationObject{
			Key:  fmt.Sprintf("endlessfs/v1/test/operation-pages/%04d.json", index),
			Body: []byte(strings.Repeat(string(rune('a'+index%26)), 2048)),
		}
	}
	operation, body, err := engine.Files().buildFileOperation(context.Background(), user, "bounded-operation", "owner", operationCopy, updates, prerequisites, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || len(body) > storageformat.MaxCanonicalBytes || operation.StepPageCount < 2 || operation.StepDigest == "" || len(operation.Roots) != 0 || len(operation.Prerequisites) != 0 || len(operation.Copies) != 0 {
		t.Fatalf("unbounded operation root = pages %d, digest %q, roots %d, prerequisites %d, copies %d, bytes %d", operation.StepPageCount, operation.StepDigest, len(operation.Roots), len(operation.Prerequisites), len(operation.Copies), len(body))
	}
	pageObjects, err := listAllFrom(context.Background(), backend, storageformat.OperationStagingPrefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(pageObjects) < len(prerequisites)+int(operation.StepPageCount) || len(pageObjects) > len(prerequisites)+int(operation.StepPageCount)+4 {
		t.Fatalf("staged operation objects = %d; want caller prerequisites, bounded directory prerequisites, and %d pages", len(pageObjects), operation.StepPageCount)
	}
	for index := uint64(0); index < operation.StepPageCount; index++ {
		key := stagedFileOperationStepPageKey(operation, index)
		object, err := backend.Get(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if len(object.Body) == 0 || len(object.Body) > storageformat.MaxCanonicalBytes {
			t.Fatalf("unbounded operation page = %d bytes", len(object.Body))
		}
		if strings.Contains(string(object.Body), strings.Repeat("a", 256)) {
			t.Fatal("operation page embedded a prerequisite body")
		}
	}
	if canonicalPages, err := listAllFrom(context.Background(), backend, storageformat.FileOperationStepPagePrefix(user.String(), operation.OperationID)); err != nil || len(canonicalPages) != 0 {
		t.Fatalf("authoritative operation pages before admission = %d, %v; want none", len(canonicalPages), err)
	}
	if _, err := backend.Get(context.Background(), objectstore.MustKey(prerequisites[0].Key)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("authoritative prerequisite exists before admission: %v", err)
	}
	if err := engine.CloseWrites(context.Background(), "operation-staging-cleanup"); err != nil {
		t.Fatal(err)
	}
	remaining, err := listAllFrom(context.Background(), backend, storageformat.OperationStagingPrefix())
	if err != nil || len(remaining) != 0 {
		t.Fatalf("closed-gate operation staging objects = %d, %v; want none", len(remaining), err)
	}
}
