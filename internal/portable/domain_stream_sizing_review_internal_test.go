package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// This oracle deliberately retains the pre-optimization growing-prefix Add
// algorithm. It does not call the selected prefix/size accounting helper. The
// shared writePage boundary still owns canonical validation and provider I/O.
type reviewOldStreamBuilder struct {
	ctx     context.Context
	session *consistencyDomainTreeSession
	leaf    []storageformat.DomainEntry
	levels  [][]domainPageRef
}

func (builder *reviewOldStreamBuilder) Add(entry storageformat.DomainEntry) error {
	candidate := append(append([]storageformat.DomainEntry(nil), builder.leaf...), entry)
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: builder.session.reference.ID, Kind: builder.session.reference.Kind, Level: 0, Entries: candidate}
	if len(candidate) <= domainPageMaximumItems {
		if _, err := storageformat.EncodeCanonical(page); err == nil {
			builder.leaf = candidate
			return nil
		}
	}
	if len(builder.leaf) == 0 {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain entry cannot fit in a canonical page")
	}
	if err := builder.flushLeaf(); err != nil {
		return err
	}
	return builder.Add(entry)
}

func (builder *reviewOldStreamBuilder) flushLeaf() error {
	if len(builder.leaf) == 0 {
		return nil
	}
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: builder.session.reference.ID, Kind: builder.session.reference.Kind, Level: 0, Entries: builder.leaf}
	reference, err := builder.session.writePage(builder.ctx, page)
	if err != nil {
		return err
	}
	builder.session.forgetPage(reference.root.Digest)
	builder.leaf = nil
	return builder.addReference(0, reference)
}

func (builder *reviewOldStreamBuilder) addReference(level int, reference domainPageRef) error {
	for len(builder.levels) <= level {
		builder.levels = append(builder.levels, nil)
	}
	builder.levels[level] = append(builder.levels[level], reference)
	if len(builder.levels[level]) < domainPageMaximumItems {
		return nil
	}
	return builder.flushLevel(level)
}

func (builder *reviewOldStreamBuilder) flushLevel(level int) error {
	references := builder.levels[level]
	if len(references) == 0 {
		return nil
	}
	children := make([]storageformat.DomainPageChild, len(references))
	for index, child := range references {
		var err error
		children[index], err = builder.session.pageChild(child)
		if err != nil {
			return err
		}
	}
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: builder.session.reference.ID, Kind: builder.session.reference.Kind, Level: level + 1, Children: children}
	reference, err := builder.session.writePage(builder.ctx, page)
	if err != nil {
		return err
	}
	builder.session.forgetPage(reference.root.Digest)
	builder.levels[level] = nil
	return builder.addReference(level+1, reference)
}

func (builder *reviewOldStreamBuilder) Finish() (storageformat.DomainTreeRoot, error) {
	if err := builder.flushLeaf(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	for {
		lowest, total, only := -1, 0, domainPageRef{}
		for level, references := range builder.levels {
			if len(references) == 0 {
				continue
			}
			if lowest < 0 {
				lowest = level
			}
			total += len(references)
			if len(references) == 1 {
				only = references[0]
			}
		}
		if total == 0 {
			return storageformat.DomainTreeRoot{}, nil
		}
		if total == 1 {
			return only.root, nil
		}
		if err := builder.flushLevel(lowest); err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
	}
}

type reviewStreamBuilder interface {
	Add(storageformat.DomainEntry) error
	Finish() (storageformat.DomainTreeRoot, error)
}

func reviewStreamEntries(count, width int) []storageformat.DomainEntry {
	entries := make([]storageformat.DomainEntry, count)
	for index := range entries {
		entry := storageformat.DomainEntry{Key: fmt.Sprintf("item/%06d-\"\\\n<测试", index), LogicalVersion: "version-\"\\\n<&>测试"}
		switch index % 6 {
		case 0:
			entry.Value = nil
		case 1:
			entry.Value = []byte{}
		default:
			entry.Value = bytes.Repeat([]byte{byte(index)}, width+index%3)
		}
		entries[index] = entry
	}
	return entries
}

type reviewStreamResult struct {
	root    storageformat.DomainTreeRoot
	objects map[string][]byte
	pages   map[string][]byte
	events  []providerbudget.Event
	model   providerbudget.Totals
}

// Both builders use fresh fixture state, the same source/preparation binding,
// clock and entries. Only immutable tree preparation is measured; no head is
// published. Readback happens outside the provider ledger.
func reviewRunStream(t *testing.T, entries []storageformat.DomainEntry, packed, old, loseFirstPut bool) reviewStreamResult {
	t.Helper()
	ctx := context.Background()
	base := objectmemory.New()
	ledger := providerbudget.NewLedger()
	objects := make(map[string][]byte)
	puts := 0
	hook := &hookedBackend{Backend: base}
	hook.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
		if !strings.Contains(key.String(), "/pages/") && !strings.Contains(key.String(), "/packs/") {
			t.Fatalf("tree builder attempted authority publication: %s", key.String())
		}
		if condition.Mode != objectstore.PutCreateOnly {
			t.Fatalf("immutable tree write condition = %+v", condition)
		}
		if prior, found := objects[key.String()]; found && !bytes.Equal(prior, body) {
			t.Fatal("retry changed bytes under the same immutable physical key")
		}
		objects[key.String()] = bytes.Clone(body)
		version, err := base.Put(ctx, key, body, condition)
		puts++
		if loseFirstPut && puts == 1 && err == nil {
			return "", domain.NewError(domain.ErrorUnavailable, "synthetic lost immutable publication response")
		}
		return version, err
	}
	backend := budgettest.Wrap(providerbudget.RoleState, hook, ledger)
	clock := domain.NewFixedClock(time.Date(2078, 2, 3, 4, 5, 6, 7, time.UTC))
	store := newConsistencyDomainStore(backend, nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "stream-sizing-review"}
	build := func() (storageformat.DomainTreeRoot, error) {
		session := newConsistencyDomainTreeSession(store, reference)
		if packed {
			session.enablePackedWrites("same-authenticated-source")
			if err := session.bindPreparedMutation("same-logical-mutation", time.Time{}); err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
		}
		var builder reviewStreamBuilder = newConsistencyDomainTreeBuilder(ctx, session)
		if old {
			builder = &reviewOldStreamBuilder{ctx: ctx, session: session}
		}
		for _, entry := range entries {
			if err := builder.Add(entry); err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
		}
		root, err := builder.Finish()
		if err == nil {
			err = session.flushPack(ctx)
		}
		return root, err
	}
	root, err := build()
	if loseFirstPut {
		if !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("lost response fixture did not reach successful immutable PUT: %v", err)
		}
		root, err = build()
	}
	if err != nil {
		t.Fatal(err)
	}
	events := ledger.Events()
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	model, err := economics.Estimate(events)
	if err != nil {
		t.Fatal(err)
	}
	pages := make(map[string][]byte)
	if packed && root.Digest != "" {
		wire := objects[storageformat.DomainPagePackKey(reference.Kind, reference.ID, root.PackID).String()]
		pack, err := storageformat.DecodeDomainPagePack(ctx, wire, reference.ID, reference.Kind, root.PackID)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range pack.Pages {
			body, err := storageformat.EncodeCanonical(member.Page)
			if err != nil {
				t.Fatal(err)
			}
			pages[member.Digest] = body
		}
	} else {
		for key, body := range objects {
			pages[key] = body
		}
	}
	reader := newConsistencyDomainTreeSession(newConsistencyDomainStore(base, nil, clock), reference)
	iterator, err := newConsistencyDomainTreeIterator(ctx, reader, root)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range entries {
		got, found, err := iterator.Next()
		if err != nil || !found || got.Key != want.Key || got.LogicalVersion != want.LogicalVersion || !bytes.Equal(got.Value, want.Value) {
			t.Fatalf("fresh independent readback entry %d differs: found=%t err=%v", index, found, err)
		}
	}
	if _, found, err := iterator.Next(); err != nil || found {
		t.Fatalf("fresh independent readback has extra entries: found=%t err=%v", found, err)
	}
	return reviewStreamResult{root: root, objects: objects, pages: pages, events: events, model: model}
}

func reviewAssertStreamEquivalent(t *testing.T, entries []storageformat.DomainEntry, packed, lost bool) {
	t.Helper()
	old := reviewRunStream(t, entries, packed, true, lost)
	selected := reviewRunStream(t, entries, packed, false, lost)
	if old.root != selected.root || !reflect.DeepEqual(old.pages, selected.pages) || !reflect.DeepEqual(old.objects, selected.objects) {
		t.Fatal("selected stream builder changed canonical pages, root or immutable provider wire")
	}
	if !reflect.DeepEqual(old.events, selected.events) || !reflect.DeepEqual(old.model, selected.model) {
		t.Fatalf("selected stream builder changed complete provider event/model vector: old=%+v selected=%+v", old.model, selected.model)
	}
	t.Logf("exact stream equivalence items=%d packed=%t lostResponse=%t pages=%d objects=%d requests=%d writtenBytes=%d readBytes=%d", len(entries), packed, lost, len(selected.pages), len(selected.objects), selected.model.Requests, selected.model.RequestBytes, selected.model.ResponseBytes)
}

func TestConsistencyDomainStreamSizingReviewProviderBudgetExactWireAndBoundaries(t *testing.T) {
	for _, packed := range []bool{false, true} {
		for _, count := range []int{0, 1, 255, 256, 257, 1024} {
			t.Run(fmt.Sprintf("packed-%t/items-%d", packed, count), func(t *testing.T) {
				reviewAssertStreamEquivalent(t, reviewStreamEntries(count, 17), packed, false)
			})
		}
		for _, width := range []int{0, 1, 2, 3, 7000} {
			t.Run(fmt.Sprintf("packed-%t/escaped-and-base64-width-%d", packed, width), func(t *testing.T) {
				reviewAssertStreamEquivalent(t, reviewStreamEntries(257, width), packed, false)
			})
		}
	}
}

func TestConsistencyDomainStreamSizingReviewProviderBudgetExactCanonicalLimit(t *testing.T) {
	entries := []storageformat.DomainEntry{
		{Key: "a-\"\\\n<测试", Value: bytes.Repeat([]byte{0xfb}, 350000), LogicalVersion: "v"},
		{Key: "b-\"\\\n<测试", Value: bytes.Repeat([]byte{0xfc}, 350000), LogicalVersion: "v"},
	}
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: "stream-sizing-review", Kind: storageformat.DomainOwnerControl, Entries: entries}
	body, err := storageformat.EncodeCanonical(page)
	if err != nil {
		t.Fatal(err)
	}
	entries[1].LogicalVersion += strings.Repeat("x", storageformat.MaxCanonicalBytes-len(body))
	body, err = storageformat.EncodeCanonical(page)
	if err != nil || len(body) != storageformat.MaxCanonicalBytes {
		t.Fatalf("exact-limit fixture = %d bytes, %v", len(body), err)
	}
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed-%t/exact-limit", packed), func(t *testing.T) {
			reviewAssertStreamEquivalent(t, entries, packed, false)
		})
		plusOne := append([]storageformat.DomainEntry(nil), entries...)
		plusOne[1].LogicalVersion += "x"
		if _, err := storageformat.EncodeCanonical(storageformat.DomainPage{SchemaVersion: 1, DomainID: page.DomainID, Kind: page.Kind, Entries: plusOne}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("limit-plus-one fixture was not over canonical bound: %v", err)
		}
		t.Run(fmt.Sprintf("packed-%t/limit-plus-one", packed), func(t *testing.T) {
			reviewAssertStreamEquivalent(t, plusOne, packed, false)
		})
	}
}

func TestConsistencyDomainStreamSizingReviewProviderBudgetLostImmutableResponse(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed-%t", packed), func(t *testing.T) {
			reviewAssertStreamEquivalent(t, reviewStreamEntries(300, 17), packed, true)
		})
	}
}

func TestConsistencyDomainStreamSizingReviewOwnsAdmittedValues(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed-%t", packed), func(t *testing.T) {
			reviewStreamOwnsAdmittedValues(t, packed)
		})
	}
}

func reviewStreamOwnsAdmittedValues(t *testing.T, packed bool) {
	t.Helper()
	ctx := context.Background()
	base := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "stream-ownership-review"}
	store := newConsistencyDomainStore(base, nil)
	session := newConsistencyDomainTreeSession(store, reference)
	if packed {
		session.enablePackedWrites("ownership-source")
		if err := session.bindPackedMutation("ownership-mutation"); err != nil {
			t.Fatal(err)
		}
	}
	builder := newConsistencyDomainTreeBuilder(ctx, session)
	entries := reviewStreamEntries(300, 17)
	want := make([]storageformat.DomainEntry, len(entries))
	for index, entry := range entries {
		want[index] = entry
		want[index].Value = bytes.Clone(entry.Value)
		if err := builder.Add(entry); err != nil {
			t.Fatal(err)
		}
		// Mutation after admission must not alter a page whose byte size has
		// already been memoized, including a page retained for packed writes.
		for offset := range entry.Value {
			entry.Value[offset] ^= 0xff
		}
	}
	root, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.flushPack(ctx); err != nil {
		t.Fatal(err)
	}
	iterator, err := newConsistencyDomainTreeIterator(ctx, newConsistencyDomainTreeSession(store, reference), root)
	if err != nil {
		t.Fatal(err)
	}
	for index, expected := range want {
		entry, found, err := iterator.Next()
		if err != nil || !found || entry.Key != expected.Key || !bytes.Equal(entry.Value, expected.Value) {
			t.Fatalf("caller mutation escaped into entry %d: found=%t err=%v", index, found, err)
		}
	}
}

func TestConsistencyDomainStreamSizingReviewCanceledAdmissionDoesNoProviderWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := objectmemory.New()
	ledger := providerbudget.NewLedger()
	store := newConsistencyDomainStore(budgettest.Wrap(providerbudget.RoleState, base, ledger), nil)
	builder := newConsistencyDomainTreeBuilder(ctx, newConsistencyDomainTreeSession(store, consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "stream-cancel-review"}))
	cancel()
	if err := builder.Add(storageformat.DomainEntry{Key: "valid", Value: []byte("valid"), LogicalVersion: "v"}); !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled admission = %v", err)
	}
	if len(ledger.Events()) != 0 {
		t.Fatal("canceled admission contacted provider")
	}
}

func TestConsistencyDomainStreamSizingReviewDenialsAndFailurePrecedence(t *testing.T) {
	for _, old := range []bool{true, false} {
		for _, scenario := range []string{"oversized-first", "oversized-after-valid-write-failure", "duplicate", "reverse", "empty-key", "invalid-utf8", "cancelled-flush"} {
			t.Run(fmt.Sprintf("old-%t/%s", old, scenario), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				puts := 0
				base := objectmemory.New()
				hook := &hookedBackend{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					puts++
					if scenario == "oversized-after-valid-write-failure" {
						return "", domain.NewError(domain.ErrorUnavailable, "synthetic leaf transport failure")
					}
					return base.Put(ctx, key, body, condition)
				}}
				session := newConsistencyDomainTreeSession(newConsistencyDomainStore(hook, nil), consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "stream-denial-review"})
				var builder reviewStreamBuilder = newConsistencyDomainTreeBuilder(ctx, session)
				if old {
					builder = &reviewOldStreamBuilder{ctx: ctx, session: session}
				}
				valid := storageformat.DomainEntry{Key: "a", Value: []byte("synthetic"), LogicalVersion: "v"}
				oversized := storageformat.DomainEntry{Key: "z", Value: make([]byte, storageformat.MaxCanonicalBytes), LogicalVersion: "v"}
				var err error
				want := error(domain.ErrInvalid)
				switch scenario {
				case "oversized-first":
					err = builder.Add(oversized)
				case "oversized-after-valid-write-failure":
					if err := builder.Add(valid); err != nil {
						t.Fatal(err)
					}
					err = builder.Add(oversized)
					want = domain.ErrUnavailable
				default:
					if err := builder.Add(valid); err != nil {
						t.Fatal(err)
					}
					next := valid
					switch scenario {
					case "reverse":
						next.Key = "0"
					case "empty-key":
						next.Key = ""
					case "invalid-utf8":
						next.Key = string([]byte{0xff})
					case "cancelled-flush":
						cancel()
						want = domain.ErrUnavailable
					}
					if scenario != "cancelled-flush" {
						if err := builder.Add(next); err != nil {
							t.Fatal(err)
						}
					}
					_, err = builder.Finish()
				}
				if !errors.Is(err, want) {
					t.Fatalf("denial kind = %v, want %v", err, want)
				}
				if scenario == "oversized-after-valid-write-failure" || scenario == "cancelled-flush" {
					if puts != 1 {
						t.Fatalf("fixture did not reach original flush boundary: puts=%d", puts)
					}
				} else if puts != 0 {
					t.Fatalf("invalid input published immutable objects: puts=%d", puts)
				}
			})
		}
	}
}
