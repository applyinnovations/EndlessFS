package portable

// Guarantee-level regression proofs for the closure layout. These exercise the
// actual runtime entry point, including repeated descriptors and partial PUTs.
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type closurePreservationFixture struct {
	source *consistencyDomainTreeSession
	roots  []storageformat.DomainTreeRoot
	leaves []domainPageRef
	values map[string][]byte
}

var closurePreservationTime = time.Date(2073, 4, 5, 6, 7, 8, 0, time.UTC)

func newClosurePreservationFixture(t *testing.T, backend objectstore.Backend, large bool, mutation string, now, deadline time.Time, lateChange byte) closurePreservationFixture {
	t.Helper()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "closure-preservation-owner"}
	source := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil, domain.NewFixedClock(now)), reference)
	source.enablePackedWrites("authenticated-original-head")
	if err := source.bindPreparedMutation(mutation, deadline); err != nil {
		t.Fatal("fixture preparation binding failed")
	}
	count := 2
	if large {
		count = 8 // Same actual-bound geometry as reviewRepackMutation, not a new limit.
	}
	fixture := closurePreservationFixture{source: source, values: make(map[string][]byte)}
	noise := &deterministicScaleReader{state: 0xa63927f1}
	children := make([]storageformat.DomainPageChild, count)
	for index := range count {
		key := fmt.Sprintf("member-%02d", index)
		value := []byte(fmt.Sprintf("original-value-%02d", index))
		if large {
			value = make([]byte, 600_000)
			if n, err := noise.Read(value); err != nil || n != len(value) {
				t.Fatal("fixture deterministic payload unavailable")
			}
		}
		if index == count-1 {
			value[0] ^= lateChange
		}
		fixture.values[key] = bytes.Clone(value)
		leaf, err := source.writePage(context.Background(), storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind,
			Entries: []storageformat.DomainEntry{{Key: key, Value: value, LogicalVersion: "original-version"}}})
		if err != nil {
			t.Fatal("fixture leaf staging failed")
		}
		fixture.leaves = append(fixture.leaves, leaf)
		children[index] = storageformat.DomainPageChild{FirstKey: leaf.firstKey, LastKey: leaf.lastKey, Digest: leaf.root.Digest, PackID: leaf.root.PackID,
			LeafKeyFilter: leaf.leafKeyFilter, Level: leaf.root.Level, EntryCount: leaf.root.EntryCount, ByteCount: leaf.root.ByteCount}
	}
	branch, err := source.writePage(context.Background(), storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 1, Children: children})
	if err != nil {
		t.Fatal("fixture parent staging failed")
	}
	// The first root memoizes a leaf. The second visits it again through a
	// complete child descriptor, then visits the independent sibling(s).
	fixture.roots = []storageformat.DomainTreeRoot{fixture.leaves[0].root, branch.root}
	return fixture
}

func closurePreservationSourceBodies(t *testing.T, pages map[string]storageformat.DomainPage) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte, len(pages))
	for digest, page := range pages {
		body, err := storageformat.EncodeCanonical(page)
		if err != nil {
			t.Fatal("fixture page encoding failed")
		}
		result[digest] = body
	}
	return result
}

func closurePreservationAssertValues(t *testing.T, backend objectstore.Backend, fixture closurePreservationFixture, roots []storageformat.DomainTreeRoot) {
	t.Helper()
	if len(roots) != len(fixture.roots) {
		t.Fatal("repacking lost a logical root")
	}
	reader := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil, domain.NewFixedClock(closurePreservationTime)), fixture.source.reference)
	for key, expected := range fixture.values {
		actual, found, err := reader.lookup(context.Background(), roots[1], key)
		if err != nil || !found || !bytes.Equal(actual.Data, expected) || actual.LogicalVersion != "original-version" {
			t.Fatal("fresh reader lost exact original value/version authority")
		}
	}
	leaf, err := reader.readPage(context.Background(), domainPageRef{root: roots[0]})
	if err != nil || len(leaf.Entries) != 1 || !bytes.Equal(leaf.Entries[0].Value, fixture.values["member-00"]) {
		t.Fatal("shared first root was not independently readable")
	}
}

func TestBoundedClosurePreservationMemoizedIncomingDescriptors(t *testing.T) {
	for _, mode := range []string{"matching-control", "level", "entry-count", "byte-count", "first-key", "last-key", "leaf-filter"} {
		t.Run(mode, func(t *testing.T) {
			base := objectmemory.New()
			backend := &reviewRepackFaultBackend{Backend: base}
			fixture := newClosurePreservationFixture(t, backend, false, "memoized-descriptor", closurePreservationTime, time.Time{}, 0)
			parent := fixture.source.pendingPack[fixture.roots[1].Digest]
			parent.Children = append([]storageformat.DomainPageChild(nil), parent.Children...)
			child := &parent.Children[0]
			switch mode {
			case "level":
				// This remains a structurally valid parent/edge. Only resolving
				// the already-memoized leaf exposes the contradictory level.
				parent.Level, child.Level = 2, 1
				child.LeafKeyFilter = ""
				parent.Children = parent.Children[:1]
			case "entry-count":
				child.EntryCount++
			case "byte-count":
				child.ByteCount++
			case "first-key":
				child.FirstKey = "alpha"
			case "last-key":
				child.LastKey = "member-00-z"
			case "leaf-filter":
				child.LeafKeyFilter = storageformat.DomainLeafKeyFilter([]string{"unrelated-member"})
				if child.LeafKeyFilter == fixture.leaves[0].leafKeyFilter {
					t.Fatal("forged filter fixture did not differ")
				}
			}
			parentRef, err := fixture.source.writePage(context.Background(), parent)
			if err != nil {
				t.Fatal("otherwise-valid forged edge could not be staged")
			}
			fixture.roots[1] = parentRef.root
			// A forged level could also be rejected by the later parent
			// rewrite validator. Require the actual collector to reject it
			// at the already-memoized incoming edge, before that later defense.
			collector := &domainClosure{source: fixture.source, indices: make(map[domainClosureIdentity]int), owned: make(map[domainClosureIdentity]storageformat.DomainPage), visiting: make(map[domainClosureIdentity]bool)}
			if err := collector.collect(context.Background(), domainPageRef{root: fixture.roots[0]}); err != nil {
				t.Fatal("otherwise-valid first root did not establish the memo positive control")
			}
			edgeErr := collector.collect(context.Background(), domainPageRef{root: fixture.roots[1]})
			if mode == "matching-control" {
				if edgeErr != nil {
					t.Fatal("valid repeated edge did not pass the collector positive control")
				}
			} else if !errors.Is(edgeErr, domain.ErrInvalid) || errors.Is(edgeErr, storageformat.ErrDomainPagePackTooLarge) {
				t.Fatal("forged incoming descriptor passed memo collection before rewrite")
			}
			roots, err := repackStagedRoots(context.Background(), fixture.source, fixture.roots)
			if mode == "matching-control" {
				if err != nil {
					t.Fatal("valid repeated DAG descriptor was rejected")
				}
				closurePreservationAssertValues(t, base, fixture, roots)
				return
			}
			if !errors.Is(err, domain.ErrInvalid) || errors.Is(err, storageformat.ErrDomainPagePackTooLarge) || backend.packPuts != 0 || len(base.Export()) != 0 {
				t.Fatal("memoized descriptor denial was bypassed, reclassified or published")
			}
		})
	}
}

func TestBoundedClosurePreservationOwnsPagesWithoutRewritingSource(t *testing.T) {
	base := objectmemory.New()
	fixture := newClosurePreservationFixture(t, base, false, "owned-original-pages", closurePreservationTime, time.Time{}, 0)
	pendingBefore := closurePreservationSourceBodies(t, fixture.source.pendingPack)
	pagesBefore := closurePreservationSourceBodies(t, fixture.source.pages)
	packID, packSeed := fixture.source.packID, fixture.source.packSeed
	roots, err := repackStagedRoots(context.Background(), fixture.source, fixture.roots)
	if err != nil {
		t.Fatal("valid ownership positive control failed")
	}
	if !reflect.DeepEqual(pendingBefore, closurePreservationSourceBodies(t, fixture.source.pendingPack)) || !reflect.DeepEqual(pagesBefore, closurePreservationSourceBodies(t, fixture.source.pages)) || fixture.source.packID != packID || fixture.source.packSeed != packSeed {
		t.Fatal("repacking mutated caller-owned page bytes or physical binding")
	}
	closurePreservationAssertValues(t, base, fixture, roots)

	// Inspect the exact collector used by repackStagedRoots. Mutating every
	// mutable source component after collection must not alter its owned view.
	closure := &domainClosure{source: fixture.source, indices: make(map[domainClosureIdentity]int), owned: make(map[domainClosureIdentity]storageformat.DomainPage), visiting: make(map[domainClosureIdentity]bool)}
	for _, root := range fixture.roots {
		if err := closure.collect(context.Background(), domainPageRef{root: root}); err != nil {
			t.Fatal("ownership collector positive control failed")
		}
	}
	ownedBefore := make(map[domainClosureIdentity][]byte)
	for identity, page := range closure.owned {
		body, err := storageformat.EncodeCanonical(page)
		if err != nil {
			t.Fatal("owned collector page encoding failed")
		}
		ownedBefore[identity] = body
	}
	for _, leaf := range fixture.leaves {
		page := fixture.source.pendingPack[leaf.root.Digest]
		page.Entries[0].Value[0] ^= 1
		page.Entries[0].LogicalVersion = "changed-after-collection"
	}
	parent := fixture.source.pendingPack[fixture.roots[1].Digest]
	parent.Children[0].FirstKey = "changed-after-collection"
	for identity, page := range closure.owned {
		body, err := storageformat.EncodeCanonical(page)
		if err != nil || !bytes.Equal(body, ownedBefore[identity]) {
			t.Fatal("owned closure aliases mutable caller entries, values or child descriptors")
		}
	}
	// Previously published bytes remain independently valid despite the later
	// deliberate corruption of this disposable caller session.
	closurePreservationAssertValues(t, base, fixture, roots)
}

type closurePreservationPriorReadGuard struct {
	objectstore.Backend
	key     objectstore.Key
	deny    bool
	denials int
}

func (guard *closurePreservationPriorReadGuard) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if guard.deny && key == guard.key {
		guard.denials++
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "synthetic forbidden prior inventory read")
	}
	return guard.Backend.Get(ctx, key)
}

func TestBoundedClosurePreservationPriorPhysicalRootAndHint(t *testing.T) {
	base := objectmemory.New()
	guard := &closurePreservationPriorReadGuard{Backend: base}
	fixture := newClosurePreservationFixture(t, guard, false, "preserve-prior-reference", closurePreservationTime, time.Time{}, 0)
	prior := newConsistencyDomainTreeSession(fixture.source.store, fixture.source.reference)
	prior.enablePackedWrites("different-authenticated-source")
	if err := prior.bindPackedMutation("different-complete-mutation"); err != nil {
		t.Fatal("prior immutable fixture binding failed")
	}
	priorRef, err := prior.writePage(context.Background(), storageformat.DomainPage{SchemaVersion: 1, DomainID: prior.reference.ID, Kind: prior.reference.Kind,
		Entries: []storageformat.DomainEntry{{Key: "prior-only", Value: []byte("prior-original-value"), LogicalVersion: "original-version"}}})
	if err != nil || prior.flushPack(context.Background()) != nil || priorRef.root.PackID == fixture.source.packID {
		t.Fatal("distinct valid prior immutable pack could not be established")
	}
	priorChild := storageformat.DomainPageChild{FirstKey: priorRef.firstKey, LastKey: priorRef.lastKey, Digest: priorRef.root.Digest, PackID: priorRef.root.PackID,
		Level: priorRef.root.Level, EntryCount: priorRef.root.EntryCount, ByteCount: priorRef.root.ByteCount}
	// Preserve a valid absent legacy hint exactly; do not opportunistically
	// restore it from a warm pack or turn prior scope into inventory work.
	parent := fixture.source.pendingPack[fixture.roots[1].Digest]
	parent.Children = append(append([]storageformat.DomainPageChild(nil), parent.Children...), priorChild)
	parentRef, err := fixture.source.writePage(context.Background(), parent)
	if err != nil {
		t.Fatal("mixed prior/staged parent could not be staged")
	}
	fixture.roots[1] = parentRef.root
	fixture.roots = append(fixture.roots, priorRef.root)
	fixture.values["prior-only"] = []byte("prior-original-value")
	guard.key, guard.deny = prior.packKey(priorRef.root.PackID), true
	roots, err := repackStagedRoots(context.Background(), fixture.source, fixture.roots)
	if err != nil || guard.denials != 0 || len(roots) != 3 || roots[2] != priorRef.root {
		t.Fatal("repacking changed or fetched an unchanged prior physical root")
	}
	guard.deny = false
	reader := newConsistencyDomainTreeSession(fixture.source.store, fixture.source.reference)
	page, err := reader.readPage(context.Background(), domainPageRef{root: roots[1]})
	if err != nil || len(page.Children) != 3 || page.Children[2] != priorChild {
		t.Fatal("repacking changed a prior descriptor or its absent leaf hint")
	}
	closurePreservationAssertValues(t, base, fixture, roots)
}

func TestBoundedClosurePreservationCompleteInputDeterministicIdentity(t *testing.T) {
	var referenceObjects map[string][]byte
	var referenceRoots []storageformat.DomainTreeRoot
	for _, mode := range []string{"reference", "reversed-map-insertion", "cold-staged-page-cache", "unreachable-staged-page", "late-value", "mutation", "preparation-time", "deadline", "root-order"} {
		t.Run(mode, func(t *testing.T) {
			base := objectmemory.New()
			mutation, now, deadline, lateChange := "complete-identity", closurePreservationTime, time.Time{}, byte(0)
			switch mode {
			case "late-value":
				lateChange = 1
			case "mutation":
				mutation += "-different"
			case "preparation-time":
				now = now.Add(time.Second)
			case "deadline":
				deadline = now.Add(terminalOperationRetention + time.Second)
			}
			fixture := newClosurePreservationFixture(t, base, false, mutation, now, deadline, lateChange)
			if mode == "reversed-map-insertion" {
				keys := make([]string, 0, len(fixture.source.pendingPack))
				for key := range fixture.source.pendingPack {
					keys = append(keys, key)
				}
				sort.Sort(sort.Reverse(sort.StringSlice(keys)))
				reordered := make(map[string]storageformat.DomainPage, len(keys))
				for _, key := range keys {
					reordered[key] = fixture.source.pendingPack[key]
				}
				fixture.source.pendingPack = reordered
			}
			if mode == "cold-staged-page-cache" {
				fixture.source.pages = make(map[string]storageformat.DomainPage)
				fixture.source.pagePacks = make(map[string]string)
			}
			if mode == "unreachable-staged-page" {
				_, err := fixture.source.writePage(context.Background(), storageformat.DomainPage{SchemaVersion: 1, DomainID: fixture.source.reference.ID, Kind: fixture.source.reference.Kind,
					Entries: []storageformat.DomainEntry{{Key: "unreachable-scratch", Value: []byte("not-authority"), LogicalVersion: "scratch-version"}}})
				if err != nil {
					t.Fatal("unreachable fixture page could not be staged")
				}
			}
			if mode == "root-order" {
				fixture.roots[0], fixture.roots[1] = fixture.roots[1], fixture.roots[0]
			}
			roots, err := repackStagedRoots(context.Background(), fixture.source, fixture.roots)
			if err != nil {
				t.Fatal("identity fixture did not publish valid packs")
			}
			objects := base.Export()
			if mode == "reference" {
				referenceObjects, referenceRoots = objects, roots
				return
			}
			if mode == "reversed-map-insertion" || mode == "cold-staged-page-cache" || mode == "unreachable-staged-page" {
				if !reflect.DeepEqual(objects, referenceObjects) || !reflect.DeepEqual(roots, referenceRoots) {
					t.Fatal("same reachable input acquired different physical identity or wire")
				}
				return
			}
			for key := range objects {
				if _, found := referenceObjects[key]; found {
					t.Fatal("different complete input reused an immutable layout pack identity")
				}
			}
		})
	}
}

type closurePreservationPut struct {
	key  objectstore.Key
	body []byte
}

// Check the actual emitted groups against the input/range identity contract.
// Leaf digests remain unchanged, and this fixture has one identifiable branch.
// No global final-partition hash or guessed compressed-size threshold is used.
func closurePreservationAssertRangeIDs(t *testing.T, fixture closurePreservationFixture, roots []storageformat.DomainTreeRoot, puts []closurePreservationPut) {
	t.Helper()
	closure := &domainClosure{source: fixture.source, indices: make(map[domainClosureIdentity]int), owned: make(map[domainClosureIdentity]storageformat.DomainPage), visiting: make(map[domainClosureIdentity]bool)}
	for _, root := range fixture.roots {
		if err := closure.collect(context.Background(), domainPageRef{root: root}); err != nil {
			t.Fatal("range identity collector failed")
		}
	}
	binding, err := closure.binding(context.Background(), fixture.roots)
	if err != nil {
		t.Fatal("range identity input binding failed")
	}
	// Object keys hash pack IDs and cannot be inverted. Obtain each expected
	// ID from authenticated returned descriptors, never by parsing the key.
	physicalIDs := make(map[objectstore.Key]string)
	visited := make(map[domainClosureIdentity]bool)
	reader := newConsistencyDomainTreeSession(fixture.source.store, fixture.source.reference)
	var walk func(domainPageRef)
	walk = func(reference domainPageRef) {
		identity := domainClosureIdentity{reference.root.PackID, reference.root.Digest}
		if visited[identity] {
			return
		}
		visited[identity] = true
		page, err := reader.readPage(context.Background(), reference)
		if err != nil {
			t.Fatal("returned range descriptor was not independently authenticated")
		}
		physicalIDs[fixture.source.packKey(reference.root.PackID)] = reference.root.PackID
		for _, child := range page.Children {
			walk(domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount},
				firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter})
		}
	}
	for _, root := range roots {
		walk(domainPageRef{root: root})
	}
	seen := make(map[int]bool)
	for _, put := range puts {
		packID, found := physicalIDs[put.key]
		if !found || packID == "" {
			t.Fatal("publication included a pack outside the final reachable roots")
		}
		pack, err := storageformat.DecodeDomainPagePack(context.Background(), put.body, fixture.source.reference.ID, fixture.source.reference.Kind, packID)
		if err != nil {
			t.Fatal("emitted group failed strict independent decoding")
		}
		positions := make([]int, 0, len(pack.Pages))
		for _, member := range pack.Pages {
			position, found := closure.indices[domainClosureIdentity{fixture.source.packID, member.Digest}]
			if member.Page.Level == 1 {
				position, found = len(closure.pages)-1, true
			}
			if !found || seen[position] {
				t.Fatal("emitted group duplicated or substituted an original node")
			}
			seen[position] = true
			positions = append(positions, position)
		}
		sort.Ints(positions)
		if len(positions) == 0 {
			t.Fatal("emitted group is empty")
		}
		first, end := positions[0], positions[len(positions)-1]+1
		if end-first != len(positions) {
			t.Fatal("emitted group is not a contiguous original postorder range")
		}
		session := newConsistencyDomainTreeSession(fixture.source.store, fixture.source.reference)
		session.enablePackedWrites(fixture.source.packSeed)
		if err := session.bindPackedMutation(binding + "/" + strconv.Itoa(first) + "/" + strconv.Itoa(end)); err != nil {
			t.Fatal("range identity binding failed")
		}
		expected, err := session.ensurePackID(closure.pages[first].original.root.Digest)
		if err != nil || expected != pack.PackID || fixture.source.packKey(expected) != put.key {
			t.Fatal("actual pack identity omitted its complete input or own original range")
		}
	}
	if len(seen) != len(closure.pages) {
		t.Fatal("final groups omitted an original closure node")
	}
}

func TestBoundedClosurePreservationLostLaterPUTExactRetryAndCollision(t *testing.T) {
	ctx := context.Background()
	referenceBase := objectmemory.New()
	referenceBackend := &reviewRepackFaultBackend{Backend: referenceBase}
	fixture := newClosurePreservationFixture(t, referenceBackend, true, "later-pack-publication", closurePreservationTime, time.Time{}, 0)
	var expectedPuts []closurePreservationPut
	referenceBackend.before = func(_ context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) error {
		if condition.Mode != objectstore.PutCreateOnly {
			t.Fatal("layout publication weakened create-only storage")
		}
		expectedPuts = append(expectedPuts, closurePreservationPut{key: key, body: bytes.Clone(body)})
		return nil
	}
	expectedRoots, err := repackStagedRoots(ctx, fixture.source, fixture.roots)
	if err != nil || len(expectedPuts) < 2 {
		t.Fatal("actual 4MiB capacity fixture did not exercise multiple final packs")
	}
	closurePreservationAssertRangeIDs(t, fixture, expectedRoots, expectedPuts)
	closurePreservationAssertValues(t, referenceBase, fixture, expectedRoots)

	t.Run("cancel-after-first-durable-put", func(t *testing.T) {
		base := objectmemory.New()
		backend := &reviewRepackFaultBackend{Backend: base}
		first := newClosurePreservationFixture(t, backend, true, "later-pack-publication", closurePreservationTime, time.Time{}, 0)
		cancelContext, cancel := context.WithCancel(ctx)
		defer cancel()
		triggered := false
		backend.after = func(_ context.Context, _ objectstore.Key, version objectstore.NativeVersion, putErr error) (objectstore.NativeVersion, error) {
			if putErr == nil && !triggered {
				triggered = true
				cancel()
			}
			return version, putErr
		}
		roots, err := repackStagedRoots(cancelContext, first.source, first.roots)
		if !triggered || !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) || roots != nil || backend.packPuts != 1 || backend.headPuts != 0 || len(base.Export()) != 1 {
			t.Fatal("cancellation lost its cause, returned roots or allowed another publication")
		}
		firstObject, err := base.Get(ctx, expectedPuts[0].key)
		if err != nil || !bytes.Equal(firstObject.Body, expectedPuts[0].body) {
			t.Fatal("canceled publication did not retain the exact first durable group")
		}
		backend.after = nil
		attempt := 0
		backend.before = func(_ context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) error {
			if attempt >= len(expectedPuts) || key != expectedPuts[attempt].key || !bytes.Equal(body, expectedPuts[attempt].body) || condition.Mode != objectstore.PutCreateOnly {
				t.Fatal("fresh retry after cancellation changed final identity, wire or order")
			}
			attempt++
			return nil
		}
		retry := newClosurePreservationFixture(t, backend, true, "later-pack-publication", closurePreservationTime, time.Time{}, 0)
		roots, err = repackStagedRoots(ctx, retry.source, retry.roots)
		if err != nil || attempt != len(expectedPuts) || !reflect.DeepEqual(roots, expectedRoots) || !reflect.DeepEqual(base.Export(), referenceBase.Export()) || backend.headPuts != 0 {
			t.Fatal("fresh retry after cancellation did not restore the exact complete layout")
		}
		after, err := base.Get(ctx, expectedPuts[0].key)
		if err != nil || after.Version != firstObject.Version {
			t.Fatal("cancellation retry overwrote an already durable immutable group")
		}
		closurePreservationAssertValues(t, base, retry, roots)
	})

	t.Run("lost-later-put", func(t *testing.T) {
		base := objectmemory.New()
		backend := &reviewRepackFaultBackend{Backend: base}
		first := newClosurePreservationFixture(t, backend, true, "later-pack-publication", closurePreservationTime, time.Time{}, 0)
		index := 0
		backend.after = func(_ context.Context, _ objectstore.Key, version objectstore.NativeVersion, putErr error) (objectstore.NativeVersion, error) {
			if putErr == nil {
				index++
				if index == 2 {
					return "", domain.NewError(domain.ErrorUnavailable, "synthetic lost later durable pack response")
				}
			}
			return version, putErr
		}
		if roots, err := repackStagedRoots(ctx, first.source, first.roots); !errors.Is(err, domain.ErrUnavailable) || roots != nil || index != 2 {
			t.Fatal("lost later PUT returned a publishable root set or missed its boundary")
		}
		versions := make(map[objectstore.Key]objectstore.NativeVersion)
		for _, put := range expectedPuts[:2] {
			object, err := base.Get(ctx, put.key)
			if err != nil || !bytes.Equal(object.Body, put.body) {
				t.Fatal("lost-response fixture did not retain the exact durable prefix")
			}
			versions[put.key] = object.Version
		}
		backend.after = nil
		attempt := 0
		backend.before = func(_ context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) error {
			if attempt >= len(expectedPuts) || key != expectedPuts[attempt].key || !bytes.Equal(body, expectedPuts[attempt].body) || condition.Mode != objectstore.PutCreateOnly {
				t.Fatal("retry changed final layout identity, exact wire or publication order")
			}
			attempt++
			return nil
		}
		retry := newClosurePreservationFixture(t, backend, true, "later-pack-publication", closurePreservationTime, time.Time{}, 0)
		roots, err := repackStagedRoots(ctx, retry.source, retry.roots)
		if err != nil || attempt != len(expectedPuts) || !reflect.DeepEqual(roots, expectedRoots) || !reflect.DeepEqual(base.Export(), referenceBase.Export()) {
			t.Fatal("fresh-session exact retry did not reconstruct the complete same durable layout")
		}
		for key, version := range versions {
			object, err := base.Get(ctx, key)
			if err != nil || object.Version != version {
				t.Fatal("exact collision retry overwrote an already durable immutable object")
			}
		}
		closurePreservationAssertValues(t, base, retry, roots)
	})

	t.Run("different-bytes-later-collision", func(t *testing.T) {
		base := objectmemory.New()
		if _, err := base.Put(ctx, expectedPuts[0].key, expectedPuts[0].body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal("exact prefix collision positive control could not be installed")
		}
		const conflicting = "synthetic conflicting immutable wire"
		if _, err := base.Put(ctx, expectedPuts[1].key, []byte(conflicting), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal("later immutable collision could not be installed")
		}
		before := base.Export()
		backend := &reviewRepackFaultBackend{Backend: base}
		retry := newClosurePreservationFixture(t, backend, true, "later-pack-publication", closurePreservationTime, time.Time{}, 0)
		roots, err := repackStagedRoots(ctx, retry.source, retry.roots)
		if !errors.Is(err, errDomainPagePackIdentityCollision) || errors.Is(err, storageformat.ErrDomainPagePackTooLarge) || roots != nil || backend.packPuts != 2 || !reflect.DeepEqual(base.Export(), before) {
			t.Fatal("different immutable bytes were accepted, resized around or overwritten")
		}
	})
}
