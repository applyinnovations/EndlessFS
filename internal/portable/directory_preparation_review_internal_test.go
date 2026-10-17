package portable

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestDirectoryPreparationReviewRevalidatesConcurrentFileAncestor(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	other := openInternalTestEngine(t, backend, engine.clock.(*domain.FixedClock), strings.NewReader(strings.Repeat("r", 1<<15)))
	live := namespaceTestScope(t, domain.AreaLive)
	paths := []domain.UserPath{domain.MustParseUserPath("/new-parent"), domain.MustParseUserPath("/new-parent/deep"), domain.MustParseUserPath("/independent")}
	interleaved := false
	var winner domain.Entry
	engine.scheduler = SchedulerFunc(func(ctx context.Context, step string) error {
		if step != StepDomainBeforeHeadCommit || interleaved {
			return nil
		}
		interleaved = true
		var err error
		winner, err = newNamespaceStore(other).publishFileWithChanges(ctx, live, paths[0], domain.ConflictFail, "", "directory-review-file-winner", namespaceRequestFingerprint("directory-review-file-winner"), storageformat.DirectoryEntry{
			Kind: domain.EntryFile, BlobID: "directory-review-file-blob", Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: other.clock.Now().UTC(),
		}, nil)
		return err
	})
	items, err := engine.Files().PrepareDirectories(ctx, live, paths)
	if err != nil || !interleaved || len(items) != len(paths) {
		t.Fatalf("concurrent preparation: items=%+v interleaved=%t err=%v", items, interleaved, err)
	}
	for index, item := range items {
		if item.Index != index || item.Path != paths[index] || item.Ready != (index == 2) {
			t.Fatalf("misbound independent result %d: %+v", index, item)
		}
		if index < 2 && item.ErrorCode != domain.ErrorConflict {
			t.Fatalf("concurrent file ancestor was not a conflict: %+v", item)
		}
	}
	actual, err := engine.Files().Stat(ctx, live, paths[0])
	if err != nil || actual.Kind != domain.EntryFile || actual.Version != winner.Version {
		t.Fatalf("preparation replaced concurrent file: actual=%+v winner=%+v err=%v", actual, winner, err)
	}
	if actual, err := engine.Files().Stat(ctx, live, paths[2]); err != nil || actual.Kind != domain.EntryDirectory {
		t.Fatalf("unrelated sibling did not publish: %+v %v", actual, err)
	}
}

func TestDirectoryPreparationReviewRejectsInvalidAuthorityBeforeProviderWork(t *testing.T) {
	for _, mode := range []string{"invalid-scope", "canceled", "trash", "duplicate", "empty"} {
		t.Run(mode, func(t *testing.T) {
			ledger := providerbudget.NewLedger()
			engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scope := namespaceTestScope(t, domain.AreaLive)
			paths := []domain.UserPath{domain.MustParseUserPath("/selected")}
			want := domain.ErrInvalid
			switch mode {
			case "invalid-scope":
				scope, want = domain.Scope{}, domain.ErrUnauthorized
			case "canceled":
				cancel()
				want = domain.ErrUnavailable
			case "trash":
				scope = namespaceTestScope(t, domain.AreaTrash)
			case "duplicate":
				paths = append(paths, paths[0])
			case "empty":
				paths = nil
			}
			ledger.Reset()
			items, err := engine.Files().PrepareDirectories(ctx, scope, paths)
			if !errors.Is(err, want) || items != nil || len(ledger.Events()) != 0 {
				t.Fatalf("invalid preparation reached provider or returned ready items: items=%+v calls=%d err=%v", items, len(ledger.Events()), err)
			}
		})
	}
}

func TestDirectoryPreparationReviewGlobalAndNestedReadFailuresPublishNothing(t *testing.T) {
	for _, mode := range []string{"head-read", "nested-read", "ids-exhausted"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			base := objectmemory.New()
			seed := openNamespaceTestEngine(t, base)
			live := namespaceTestScope(t, domain.AreaLive)
			for _, path := range []string{"/existing", "/existing/child", "/unrelated-existing"} {
				if _, err := seed.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
					t.Fatal(err)
				}
			}
			store := newNamespaceStore(seed)
			view, err := store.loadView(ctx, live.UserID(), "")
			if err != nil {
				t.Fatal(err)
			}
			existing, found, err := store.child(ctx, view, view.roots[domain.AreaLive], "existing")
			if err != nil || !found || existing.Children.PackID == "" {
				t.Fatalf("nested packed read fixture: found=%t err=%v", found, err)
			}
			headKey := storageformat.DomainHeadKey(storageformat.DomainNamespace, live.UserID().String())
			before, err := base.Get(ctx, headKey)
			if err != nil {
				t.Fatal(err)
			}
			faultKey := headKey
			if mode == "nested-read" {
				faultKey = storageformat.DomainPagePackKey(storageformat.DomainNamespace, live.UserID().String(), existing.Children.PackID)
			}
			rootPackKey := storageformat.DomainPagePackKey(storageformat.DomainNamespace, live.UserID().String(), view.roots[domain.AreaLive].Children.PackID)
			if mode == "nested-read" && rootPackKey == faultKey {
				t.Fatal("nested read fixture shares the root child pack")
			}
			backend := &hookedBackend{Backend: base}
			ledger := providerbudget.NewLedger()
			engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, backend, ledger))
			triggered := false
			var rootRead atomic.Bool
			backend.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == faultKey {
					triggered = true
					return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "synthetic directory read interruption")
				}
				object, err := base.Get(ctx, key)
				if err == nil && key == rootPackKey {
					rootRead.Store(true)
				}
				return object, err
			}
			if mode == "ids-exhausted" {
				engine.ids = domain.NewIDGenerator(strings.NewReader(""))
			}
			ledger.Reset()
			paths := []domain.UserPath{domain.MustParseUserPath("/existing/new-child"), domain.MustParseUserPath("/existing/new-child/grandchild"), domain.MustParseUserPath("/independent")}
			items, err := engine.Files().PrepareDirectories(ctx, live, paths)
			if err == nil || items != nil || mode != "ids-exhausted" && (!triggered || !errors.Is(err, domain.ErrUnavailable)) {
				t.Fatalf("uncertain preparation returned ready items: triggered=%t items=%+v err=%v", triggered, items, err)
			}
			if mode == "nested-read" && !rootRead.Load() {
				t.Fatal("nested fault occurred before successful root child traversal")
			}
			for _, event := range ledger.Events() {
				if event.Kind == providerbudget.RequestObjectPut {
					t.Fatalf("uncertain preparation wrote immutable or authoritative state: %+v", event)
				}
			}
			after, err := base.Get(ctx, headKey)
			if err != nil || after.Version != before.Version {
				t.Fatalf("uncertain preparation changed the visibility head: %v", err)
			}
			backend.get = nil
			for _, path := range paths {
				if _, err := engine.Files().Stat(ctx, live, path); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("uncertain preparation made %s visible: %v", path, err)
				}
			}
			engine.ids = domain.NewIDGenerator(&deterministicScaleReader{state: 0x93481577})
			retried, err := engine.Files().PrepareDirectories(ctx, live, paths)
			if err != nil || len(retried) != len(paths) {
				t.Fatalf("safe retry after restored reads/ID source: items=%+v err=%v", retried, err)
			}
			for _, item := range retried {
				if !item.Ready || item.ErrorCode != "" {
					t.Fatalf("retry lost an independent or existing-parent descendant: %+v", item)
				}
			}
		})
	}
}

func TestDirectoryPreparationReviewLostSuccessRetriesWithoutNewPublication(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	engine := openNamespaceTestEngine(t, backend)
	live := namespaceTestScope(t, domain.AreaLive)
	paths := []domain.UserPath{domain.MustParseUserPath("/parent/child"), domain.MustParseUserPath("/parent"), domain.MustParseUserPath("/sibling")}
	lost := false
	engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepDomainAfterHeadCommit && !lost {
			lost = true
			return domain.NewError(domain.ErrorUnavailable, "synthetic lost response")
		}
		return nil
	})
	if _, err := engine.Files().PrepareDirectories(ctx, live, paths); err != nil && !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unexpected lost-response result: %v", err)
	}
	if !lost {
		t.Fatal("lost-success boundary was not reached")
	}
	engine.scheduler = nil
	before, err := engine.Files().Stat(ctx, live, paths[0])
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	items, err := engine.Files().PrepareDirectories(ctx, live, paths)
	if err != nil || len(items) != len(paths) {
		t.Fatalf("ensure retry: %+v %v", items, err)
	}
	for _, item := range items {
		if !item.Ready || item.ErrorCode != "" {
			t.Fatalf("retry failed an existing directory: %+v", item)
		}
	}
	for _, event := range ledger.Events() {
		if event.Kind != providerbudget.RequestObjectGet {
			t.Fatalf("lost-success retry wrote provider state: %+v", event)
		}
	}
	after, err := engine.Files().Stat(ctx, live, paths[0])
	if err != nil || before.Version != after.Version {
		t.Fatalf("lost-success retry changed identity: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestDirectoryPreparationReviewFailedPublicationExposesNoReadySibling(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	paths := []domain.UserPath{domain.MustParseUserPath("/parent"), domain.MustParseUserPath("/parent/child"), domain.MustParseUserPath("/sibling")}
	engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepDomainBeforeHeadCommit {
			return domain.NewError(domain.ErrorUnavailable, "synthetic pre-publication interruption")
		}
		return nil
	})
	if _, err := engine.Files().PrepareDirectories(ctx, live, paths); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted preparation returned success: %v", err)
	}
	engine.scheduler = nil
	for _, path := range paths {
		if _, err := engine.Files().Stat(ctx, live, path); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("unpublished directory %s became visible: %v", path, err)
		}
	}
	items, err := engine.Files().PrepareDirectories(ctx, live, paths)
	if err != nil || len(items) != len(paths) {
		t.Fatalf("retry of interrupted preparation: %+v %v", items, err)
	}
	for _, item := range items {
		if !item.Ready {
			t.Fatalf("retry did not prepare sibling: %+v", item)
		}
	}
}
