package storageformat

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

// All tests below are authored proposal controls, not executed evidence.
func decodedMemoReviewFixture(t *testing.T) (DomainPagePack, []byte) {
	t.Helper()
	pack := testDomainPagePack(t)
	leaf := pack.Pages[0].Page
	leaf.Entries = []DomainEntry{
		{Key: "alpha", Value: nil, LogicalVersion: "version-a"},
		{Key: "beta", Value: []byte{}, LogicalVersion: "version-b"},
		{Key: "gamma", Value: bytes.Repeat([]byte("opaque metadata <>&"), 8192), LogicalVersion: "version-c"},
	}
	second := DomainPage{SchemaVersion: 1, DomainID: pack.DomainID, Kind: pack.Kind, Entries: []DomainEntry{{Key: "omega", Value: []byte("second value"), LogicalVersion: "version-d"}}}
	unused := DomainPage{SchemaVersion: 1, DomainID: pack.DomainID, Kind: pack.Kind, Entries: []DomainEntry{{Key: "unused", Value: []byte("unconsumed authority"), LogicalVersion: "version-e"}}}
	leafBody, err := EncodeCanonical(leaf)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := EncodeCanonical(second)
	if err != nil {
		t.Fatal(err)
	}
	unusedBody, err := EncodeCanonical(unused)
	if err != nil {
		t.Fatal(err)
	}
	branch := DomainPage{SchemaVersion: 1, DomainID: pack.DomainID, Kind: pack.Kind, Level: 1, Children: []DomainPageChild{
		{FirstKey: "alpha", LastKey: "gamma", Digest: Digest(leafBody), PackID: pack.PackID, Level: 0, EntryCount: 3, ByteCount: uint64(len(leaf.Entries[2].Value)), LeafKeyFilter: DomainLeafKeyFilter([]string{"alpha", "beta", "gamma"})},
		{FirstKey: "omega", LastKey: "omega", Digest: Digest(secondBody), PackID: pack.PackID, Level: 0, EntryCount: 1, ByteCount: uint64(len(second.Entries[0].Value)), LeafKeyFilter: DomainLeafKeyFilter([]string{"omega"})},
	}}
	branchBody, err := EncodeCanonical(branch)
	if err != nil {
		t.Fatal(err)
	}
	pack.Pages = []DomainPackedPage{{Digest: Digest(leafBody), Page: leaf}, {Digest: Digest(secondBody), Page: second}, {Digest: Digest(branchBody), Page: branch}, {Digest: Digest(unusedBody), Page: unused}}
	wire, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	// Independent existing strict API supplies canonical order and positive
	// validity. The memo never constructs this expected decoded result.
	pack, err = DecodeDomainPagePack(context.Background(), wire, pack.DomainID, pack.Kind, pack.PackID)
	if err != nil {
		t.Fatal(err)
	}
	return pack, wire
}

func decodedMemoReviewRead(ctx context.Context, memo *decodedPackMemo, wire []byte, pack DomainPagePack, proof *DomainPagePackWireProof, own bool) (DomainPagePack, []byte, DomainPagePackWireProof, error) {
	return materializeDecodedDomainPack(ctx, wire, pack.DomainID, pack.Kind, pack.PackID, proof, own, memo)
}

func decodedMemoReviewMutate(pack *DomainPagePack) {
	pack.DomainID, pack.PackID, pack.Kind = "changed-owner", "changed-pack", DomainAdmin
	for index := range pack.Pages {
		packed := &pack.Pages[index]
		packed.Digest = "changed-digest"
		packed.Page.DomainID, packed.Page.Kind, packed.Page.Level = "changed-owner", DomainShare, 99
		for entry := range packed.Page.Entries {
			value := &packed.Page.Entries[entry]
			value.Key, value.LogicalVersion = "changed-key", "changed-version"
			if len(value.Value) != 0 {
				value.Value[0] ^= 0x40
			} else {
				value.Value = append(value.Value, 1)
			}
		}
		for child := range packed.Page.Children {
			value := &packed.Page.Children[child]
			value.FirstKey, value.LastKey, value.Digest, value.PackID, value.LeafKeyFilter = "changed-first", "changed-last", "changed-digest", "changed-pack", "changed-filter"
			value.Level, value.EntryCount, value.ByteCount = 99, 99, 99
		}
	}
}

// Audit all mutable slice fields recursively, including future struct fields.
// New maps/pointers/interfaces require an explicit clone/charge review instead
// of silently inheriting shallow ownership through a struct assignment.
func decodedMemoReviewIndependent(t *testing.T, first, second reflect.Value) {
	t.Helper()
	if first.Type() != second.Type() {
		t.Fatal("clone changed result type")
	}
	switch first.Kind() {
	case reflect.Struct:
		for index := 0; index < first.NumField(); index++ {
			decodedMemoReviewIndependent(t, first.Field(index), second.Field(index))
		}
	case reflect.Slice:
		if first.Len() != second.Len() || first.IsNil() != second.IsNil() {
			t.Fatal("clone changed slice shape")
		}
		if first.Len() != 0 && first.Pointer() == second.Pointer() {
			t.Fatal("clone shares mutable backing storage")
		}
		if first.Type().Elem().Kind() == reflect.Struct || first.Type().Elem().Kind() == reflect.Slice {
			for index := 0; index < first.Len(); index++ {
				decodedMemoReviewIndependent(t, first.Index(index), second.Index(index))
			}
		}
	case reflect.Map, reflect.Pointer, reflect.Interface, reflect.UnsafePointer:
		t.Fatal("new reference field requires a reviewed ownership and charge model")
	}
}

func decodedMemoReviewFieldInventory(t *testing.T) {
	t.Helper()
	for _, fixture := range []struct {
		value  any
		fields []string
	}{
		{DomainPagePack{}, []string{"SchemaVersion", "DomainID", "Kind", "PackID", "Pages"}},
		{DomainPackedPage{}, []string{"Digest", "Page"}},
		{DomainPage{}, []string{"SchemaVersion", "DomainID", "Kind", "Level", "Entries", "Children"}},
		{DomainEntry{}, []string{"Key", "Value", "LogicalVersion"}},
		{DomainPageChild{}, []string{"FirstKey", "LastKey", "Digest", "PackID", "LeafKeyFilter", "Level", "EntryCount", "ByteCount"}},
	} {
		typeOf := reflect.TypeOf(fixture.value)
		if typeOf.NumField() != len(fixture.fields) {
			t.Fatal("new decoder field needs explicit clone/charge review")
		}
		for index, field := range fixture.fields {
			if typeOf.Field(index).Name != field {
				t.Fatal("changed decoder field needs explicit clone/charge review")
			}
		}
	}
}

func TestDomainPagePackDecodedMemoReviewOwnsEveryCallerAndPreservesNilEmpty(t *testing.T) {
	decodedMemoReviewFieldInventory(t)
	expected, pristine := decodedMemoReviewFixture(t)
	memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
	input := bytes.Clone(pristine)
	first, owned, proof, err := decodedMemoReviewRead(context.Background(), &memo, input, expected, nil, true)
	if err != nil || !reflect.DeepEqual(first, expected) || memo.count != 1 || memo.misses != 1 || memo.hits != 0 || len(owned) == 0 || &owned[0] == &input[0] {
		t.Fatal("first strict result did not issue independent owned wire and a sealed snapshot")
	}
	decodedMemoReviewIndependent(t, reflect.ValueOf(first), reflect.ValueOf(memo.entries[0].pack))
	for index := range input {
		input[index] ^= 0x40
	}
	decodedMemoReviewMutate(&first)
	for range 3 {
		fresh, _, _, err := decodedMemoReviewRead(context.Background(), &memo, owned, expected, &proof, false)
		if err != nil || !reflect.DeepEqual(fresh, expected) {
			t.Fatal("caller/provider mutations escaped into retained ownership")
		}
		decodedMemoReviewIndependent(t, reflect.ValueOf(fresh), reflect.ValueOf(memo.entries[0].pack))
		canonical, err := EncodeDomainPagePack(fresh)
		if err != nil || !bytes.Equal(canonical, pristine) {
			t.Fatal("memo clone differs from exact strict canonical wire")
		}
		foundNil, foundEmpty := false, false
		for _, packed := range fresh.Pages {
			for _, entry := range packed.Page.Entries {
				if entry.Key == "alpha" {
					foundNil = entry.Value == nil
				}
				if entry.Key == "beta" {
					foundEmpty = entry.Value != nil && len(entry.Value) == 0
				}
			}
		}
		if !foundNil || !foundEmpty {
			t.Fatal("nil and nonnil empty Value were conflated")
		}
		decodedMemoReviewMutate(&fresh)
	}
	if memo.hits != 3 || !bytes.Equal(owned, pristine) || memo.charged > memo.maximum {
		t.Fatal("expected warm clones or bounded unchanged owned wire were not retained")
	}
	for index := 0; index < reflect.TypeOf(proof).NumField(); index++ {
		switch reflect.TypeOf(proof).Field(index).Type.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			t.Fatal("returned wire proof could pin an evicted decoded snapshot")
		}
	}
}

func TestDomainPagePackDecodedMemoReviewWarmProofAndWireDenialsRemainStrict(t *testing.T) {
	pack, wire := decodedMemoReviewFixture(t)
	memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
	_, owned, proof, err := decodedMemoReviewRead(context.Background(), &memo, wire, pack, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Clone(owned)
	changed[len(changed)-1] ^= 0x40
	wrongDigest, wrongSize, wrongOwner, wrongKind, wrongPack := proof, proof, proof, proof, proof
	wrongDigest.digest[0] ^= 0xff
	wrongSize.size--
	wrongOwner.domainID = "other-owner"
	wrongKind.kind = DomainShare
	wrongPack.packID = Digest([]byte("other-pack"))
	for _, fixture := range []struct {
		name   string
		wire   []byte
		owner  string
		kind   ConsistencyDomainKind
		packID string
		proof  DomainPagePackWireProof
	}{
		{"zero-proof", owned, pack.DomainID, pack.Kind, pack.PackID, DomainPagePackWireProof{}},
		{"proof-digest", owned, pack.DomainID, pack.Kind, pack.PackID, wrongDigest},
		{"proof-length", owned, pack.DomainID, pack.Kind, pack.PackID, wrongSize},
		{"proof-owner", owned, pack.DomainID, pack.Kind, pack.PackID, wrongOwner},
		{"proof-kind", owned, pack.DomainID, pack.Kind, pack.PackID, wrongKind},
		{"proof-pack", owned, pack.DomainID, pack.Kind, pack.PackID, wrongPack},
		{"owner", owned, "other-owner", pack.Kind, pack.PackID, proof},
		{"kind", owned, pack.DomainID, DomainShare, pack.PackID, proof},
		{"pack", owned, pack.DomainID, pack.Kind, Digest([]byte("other-pack")), proof},
		{"same-length-wire", changed, pack.DomainID, pack.Kind, pack.PackID, proof},
		{"truncated", owned[:len(owned)-1], pack.DomainID, pack.Kind, pack.PackID, proof},
		{"oversized", make([]byte, MaxDomainPagePackBytes+1), pack.DomainID, pack.Kind, pack.PackID, proof},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			before := memo.hits
			decoded, returned, issued, err := materializeDecodedDomainPack(context.Background(), fixture.wire, fixture.owner, fixture.kind, fixture.packID, &fixture.proof, false, &memo)
			if !errors.Is(err, domain.ErrInvalid) || !reflect.DeepEqual(decoded, DomainPagePack{}) || returned != nil || issued != (DomainPagePackWireProof{}) || memo.hits != before {
				t.Fatal("globally warm memo masked an invalid proof/wire/binding or issued authority")
			}
		})
	}
	if decoded, _, _, err := decodedMemoReviewRead(context.Background(), &memo, owned, pack, &proof, false); err != nil || !reflect.DeepEqual(decoded, pack) {
		t.Fatal("denied caller invalidated an independent valid memo/proof")
	}
	// Fresh provider callers supply no wire proof. The global table's own key
	// must still bind every expected identity, even for an identical wire hash.
	for _, binding := range []struct {
		owner  string
		kind   ConsistencyDomainKind
		packID string
	}{
		{"other-owner", pack.Kind, pack.PackID},
		{pack.DomainID, DomainShare, pack.PackID},
		{pack.DomainID, pack.Kind, Digest([]byte("other-pack"))},
	} {
		before := memo.hits
		decoded, returned, issued, err := materializeDecodedDomainPack(context.Background(), owned, binding.owner, binding.kind, binding.packID, nil, true, &memo)
		if !errors.Is(err, domain.ErrInvalid) || !reflect.DeepEqual(decoded, DomainPagePack{}) || returned != nil || issued != (DomainPagePackWireProof{}) || memo.hits != before {
			t.Fatal("fresh-provider memo lookup omitted an expected identity binding")
		}
	}
	before := memo.hits
	decoded, returned, issued, err := decodedMemoReviewRead(context.Background(), &memo, changed, pack, nil, true)
	if !errors.Is(err, domain.ErrInvalid) || !reflect.DeepEqual(decoded, DomainPagePack{}) || returned != nil || issued != (DomainPagePackWireProof{}) || memo.hits != before {
		t.Fatal("fresh-provider lookup reused a warm snapshot for corrupt same-length wire")
	}
}

func TestDomainPagePackDecodedMemoReviewColdStrictDenialsNeverBecomeSnapshots(t *testing.T) {
	pack, wire := decodedMemoReviewFixture(t)
	canonical, err := EncodeCanonical(pack)
	if err != nil {
		t.Fatal(err)
	}
	unusedNeedle := []byte(base64.StdEncoding.EncodeToString([]byte("unconsumed authority")))
	if bytes.Count(canonical, unusedNeedle) != 1 {
		t.Fatal("unused member fixture does not contain its independent base64 value")
	}
	unusedCorrupt := bytes.Replace(canonical, unusedNeedle, []byte(base64.StdEncoding.EncodeToString([]byte("unconsumed corrupt!"))), 1)
	for _, fixture := range []struct {
		name string
		wire []byte
	}{
		{"bad-json", encodeDomainPackPayload(t, []byte("{"))},
		{"unknown-field", encodeDomainPackPayload(t, bytes.Replace(canonical, []byte(`"schemaVersion":1`), []byte(`"unknown":1,"schemaVersion":1`), 1))},
		{"duplicate-field", encodeDomainPackPayload(t, bytes.Replace(canonical, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1))},
		{"unused-member", encodeDomainPackPayload(t, unusedCorrupt)},
		{"expanded-bound", encodeDomainPackPayload(t, bytes.Repeat([]byte("x"), MaxExpandedDomainPagePackBytes+1))},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if bytes.Equal(fixture.wire, wire) {
				t.Fatal("denial fixture did not change wire")
			}
			if _, err := DecodeDomainPagePack(context.Background(), fixture.wire, pack.DomainID, pack.Kind, pack.PackID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatal("strict oracle did not reject the intended negative fixture")
			}
			memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
			for range 2 {
				decoded, owned, proof, err := decodedMemoReviewRead(context.Background(), &memo, fixture.wire, pack, nil, true)
				if !errors.Is(err, domain.ErrInvalid) || owned != nil || proof != (DomainPagePackWireProof{}) || !reflect.DeepEqual(decoded, DomainPagePack{}) || memo.count != 0 || memo.hits != 0 {
					t.Fatal("invalid or unconsumed authority was memoized or exposed")
				}
			}
		})
	}
}

func TestDomainPagePackDecodedMemoReviewAdmissionCancellationAndCloneBoundaries(t *testing.T) {
	pack, wire := decodedMemoReviewFixture(t)
	memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
	_, owned, proof, err := decodedMemoReviewRead(context.Background(), &memo, wire, pack, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	// Occupy the exact existing slot. No memo copy/hash/clone occurs before it.
	domainPagePackDecodeSlot <- struct{}{}
	base, cancel := context.WithCancel(context.Background())
	waiting := &packAdmissionWaitingContext{Context: base, ready: make(chan struct{})}
	finished := make(chan error, 1)
	before := memo.hits
	go func() {
		decoded, owned, issued, err := decodedMemoReviewRead(waiting, &memo, wire, pack, nil, true)
		if !reflect.DeepEqual(decoded, DomainPagePack{}) || owned != nil || issued != (DomainPagePackWireProof{}) {
			finished <- errors.New("canceled waiter exposed result authority")
			return
		}
		finished <- err
	}()
	<-waiting.ready
	cancel()
	err = <-finished
	if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) || memo.hits != before || len(domainPagePackDecodeSlot) != 1 {
		<-domainPagePackDecodeSlot
		t.Fatal("memo bypassed/corrupted occupied cancellable admission")
	}
	<-domainPagePackDecodeSlot
	counting := &cancelPackExpansionContext{Context: context.Background()}
	if _, _, _, err := decodedMemoReviewRead(counting, &memo, owned, pack, &proof, false); err != nil {
		t.Fatal(err)
	}
	for boundary := 1; boundary <= counting.checks; boundary++ {
		ctx := &cancelPackExpansionContext{Context: context.Background(), cancelAt: boundary}
		decoded, returned, issued, err := decodedMemoReviewRead(ctx, &memo, owned, pack, &proof, false)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) || !reflect.DeepEqual(decoded, DomainPagePack{}) || returned != nil || issued != (DomainPagePackWireProof{}) {
			t.Fatalf("cancellation boundary%d returned partial authority", boundary)
		}
	}
	if decoded, _, _, err := decodedMemoReviewRead(context.Background(), &memo, owned, pack, &proof, false); err != nil || !reflect.DeepEqual(decoded, pack) || len(domainPagePackDecodeSlot) != 0 {
		t.Fatal("independent cancellation poisoned memo or leaked admission")
	}
}

func TestDomainPagePackDecodedMemoReviewChargeInputBounds(t *testing.T) {
	for _, invalid := range []struct {
		count int
		size  uintptr
	}{
		{-1, 8},
		{1, 0},
		{int(^uint(0) >> 1), 8},
		{1, ^uintptr(0)},
	} {
		if charge := decodedArrayCharge(invalid.count, invalid.size); charge <= decodedPackMemoMaximumBytes {
			t.Fatal("invalid or overflowing geometry was admitted")
		}
	}
	if decodedArrayCharge(0, 8) != 0 || decodedArrayCharge(1, 1) != 32 || decodedArrayCharge(3, 16) != 64 {
		t.Fatal("bounded empty or ordinary allocation charge changed")
	}
}

func TestDomainPagePackDecodedMemoReviewChargeEvictionAndIndependentWireProof(t *testing.T) {
	pack, wire := decodedMemoReviewFixture(t)
	charge, err := decodedDomainPackCharge(context.Background(), pack)
	if err != nil || charge == 0 {
		t.Fatal("valid fixture charge unavailable")
	}
	// Exactly one fixture fits. A new pack identity must evict, not overcharge.
	memo := newDecodedPackMemo(decodedPackMemoBaseCharge + charge)
	_, owned, firstProof, err := decodedMemoReviewRead(context.Background(), &memo, wire, pack, nil, true)
	if err != nil || memo.count != 1 || memo.charged != decodedPackMemoBaseCharge+charge {
		t.Fatal("first charged snapshot missing")
	}
	other := pack
	other.PackID = Digest([]byte("independent-same-sized-pack"))
	otherWire, err := EncodeDomainPagePack(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := decodedMemoReviewRead(context.Background(), &memo, otherWire, other, nil, true); err != nil || memo.count != 1 || memo.evicted != 1 || memo.charged > memo.maximum {
		t.Fatal("retained charge or exact-entry eviction is incorrect")
	}
	beforeMisses := memo.misses
	if decoded, _, _, err := decodedMemoReviewRead(context.Background(), &memo, owned, pack, &firstProof, false); err != nil || !reflect.DeepEqual(decoded, pack) || memo.misses != beforeMisses+1 {
		t.Fatal("eviction invalidated the independent wire proof or skipped required reconstruction")
	}
	declining := newDecodedPackMemo(decodedPackMemoBaseCharge + charge - 1)
	for range 2 {
		if decoded, _, _, err := decodedMemoReviewRead(context.Background(), &declining, wire, pack, nil, true); err != nil || !reflect.DeepEqual(decoded, pack) || declining.count != 0 || declining.charged != decodedPackMemoBaseCharge {
			t.Fatal("oversized retained snapshot changed strict result or escaped its charged budget")
		}
	}
	if declining.declined != 2 || declining.hits != 0 {
		t.Fatal("declined retention was silently counted as reusable")
	}
	t.Logf("DECODED_MEMO_CHARGE wire=%d charged_owned=%d fixed_table=%d budget=%d", len(wire), charge, decodedPackMemoBaseCharge, decodedPackMemoMaximumBytes)
}

func TestDomainPagePackDecodedMemoReviewConcurrentCallersOwnResults(t *testing.T) {
	pack, wire := decodedMemoReviewFixture(t)
	memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
	_, owned, proof, err := decodedMemoReviewRead(context.Background(), &memo, wire, pack, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 12
	var wait sync.WaitGroup
	errorsByCaller := make([]error, callers)
	for index := range callers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			decoded, _, _, err := decodedMemoReviewRead(context.Background(), &memo, owned, pack, &proof, false)
			if err != nil {
				errorsByCaller[index] = err
				return
			}
			if !reflect.DeepEqual(decoded, pack) {
				errorsByCaller[index] = errors.New("concurrent caller inherited mutation")
				return
			}
			decodedMemoReviewMutate(&decoded)
		}(index)
	}
	wait.Wait()
	for _, err := range errorsByCaller {
		if err != nil {
			t.Fatal(err)
		}
	}
	if decoded, _, _, err := decodedMemoReviewRead(context.Background(), &memo, owned, pack, &proof, false); err != nil || !reflect.DeepEqual(decoded, pack) || memo.charged > memo.maximum || memo.count != 1 {
		t.Fatal("concurrent caller mutations escaped private snapshots")
	}
}

func TestDomainPagePackDecodedMemoReviewEntryLimitAndLateCanceledIssuance(t *testing.T) {
	pack := testDomainPagePack(t)
	memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
	for index := range decodedPackMemoMaximumEntries + 1 {
		pack.PackID = Digest([]byte("entry-bound-" + strconv.Itoa(index)))
		wire, err := EncodeDomainPagePack(pack)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := decodedMemoReviewRead(context.Background(), &memo, wire, pack, nil, true); err != nil || memo.count > decodedPackMemoMaximumEntries || memo.charged > memo.maximum {
			t.Fatal("entry cardinality or charged memory exceeded its bound")
		}
	}
	if memo.count != decodedPackMemoMaximumEntries || memo.evicted != 1 {
		t.Fatal("entry bound did not evict exactly the oldest snapshot")
	}
	full, wire := decodedMemoReviewFixture(t)
	counting := &cancelPackExpansionContext{Context: context.Background()}
	control := newDecodedPackMemo(decodedPackMemoMaximumBytes)
	if _, _, _, err := decodedMemoReviewRead(counting, &control, wire, full, nil, true); err != nil || control.count != 1 {
		t.Fatal("cold issuance positive control failed")
	}
	// These final checks cover snapshot construction and the last publication
	// boundary. Failure must not admit a newly prepared result or value proof.
	for boundary := max(1, counting.checks-4); boundary <= counting.checks; boundary++ {
		memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
		ctx := &cancelPackExpansionContext{Context: context.Background(), cancelAt: boundary}
		decoded, owned, proof, err := decodedMemoReviewRead(ctx, &memo, wire, full, nil, true)
		if !errors.Is(err, context.Canceled) || owned != nil || proof != (DomainPagePackWireProof{}) || !reflect.DeepEqual(decoded, DomainPagePack{}) || memo.count != 0 {
			t.Fatal("late failed issuance admitted a snapshot or exposed partial authority")
		}
	}
}

func TestDomainPagePackDecodedMemoReviewDoesNotCopyProviderWireWhileQueued(t *testing.T) {
	// The maximum provider wire exists before accounting. This call must never
	// reach the copy/hash/clone path while another decoder holds admission.
	input := make([]byte, MaxDomainPagePackBytes)
	copy(input, domainPagePackMagic)
	domainPagePackDecodeSlot <- struct{}{}
	defer func() { <-domainPagePackDecodeSlot }()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &reviewWireProofWaitingContext{Context: base, waiting: make(chan struct{})}
	finished := make(chan error, 1)
	var before, blocked runtime.MemStats
	runtime.ReadMemStats(&before)
	go func() {
		pack, owned, proof, err := DecodeDomainPagePackWithDecodedMemo(ctx, input, "queued-owner", DomainNamespace, "queued-pack")
		if !reflect.DeepEqual(pack, DomainPagePack{}) || owned != nil || proof != (DomainPagePackWireProof{}) {
			finished <- errors.New("queued materialization exposed partial authority")
			return
		}
		finished <- err
	}()
	<-ctx.waiting
	runtime.ReadMemStats(&blocked)
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) {
		t.Fatal("queued materialization changed cancellation identity")
	}
	allocated := blocked.TotalAlloc - before.TotalAlloc
	t.Logf("queued materialization allocated=%d wire=%d", allocated, len(input))
	if allocated >= uint64(len(input)) || len(domainPagePackDecodeSlot) != 1 {
		t.Fatal("queued materialization copied provider wire or consumed another decoder's slot")
	}
}

func TestDomainPagePackDecodedMemoReviewTelemetryHasClosedPrivateFreeLabels(t *testing.T) {
	pack, wire := decodedMemoReviewFixture(t)
	memo := newDecodedPackMemo(decodedPackMemoMaximumBytes)
	observer := telemetry.New(nil, nil)
	ctx := telemetry.WithRequestID(telemetry.Context(context.Background(), observer), "memo-review-request-42")
	_, owned, proof, err := decodedMemoReviewRead(ctx, &memo, wire, pack, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := decodedMemoReviewRead(ctx, &memo, owned, pack, &proof, false); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	text := output.String()
	for _, expected := range []string{
		`endlessfs_operations_total{operation="metadata.pack.materialize",role="state",result="success"} 2`,
		`endlessfs_operations_total{operation="metadata.pack.decode",role="state",result="success"} 1`,
		`endlessfs_operations_total{operation="metadata.pack.admission",role="state",result="success"} 2`,
		`endlessfs_operations_active{operation="metadata.pack.materialize",role="state"} 0`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatal("closed materialize/decode/admission count missing")
		}
	}
	for _, private := range []string{pack.DomainID, pack.PackID, "unconsumed authority", "memo-review-request-42", "requestID=", "traceID=", "stage=", "reason="} {
		if strings.Contains(text, private) {
			t.Fatal("private identity or unbounded diagnostic leaked to metric labels")
		}
	}
}
