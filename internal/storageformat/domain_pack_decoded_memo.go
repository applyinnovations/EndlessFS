package storageformat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"unsafe"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

// The versioned decoder-result mechanism preserves the strict/proven reference
// APIs. Its retained storage is independent of the unchanged expansion bound.
// The memo proves only the pure decoder result for exact wire and bindings; it
// proves no provider presence, authorization, reachability, head or publication.
// The representative 4,001-item deep-path fixture churned107 misses/106
// evictions at32MiB.64MiB reduced that to17/13;128MiB saved one further miss
// with no demonstrated latency benefit. This is charged retained storage,
// not a total heap/RSS bound or permission to expand larger stored packs.
const decodedPackMemoMaximumBytes = 64 << 20
const decodedPackMemoMaximumEntries = 64

type decodedPackMemoKey struct {
	digest   [sha256.Size]byte
	wireSize int
	domainID string
	kind     ConsistencyDomainKind
	packID   string
}

type decodedPackMemoEntry struct {
	digest   [sha256.Size]byte
	wireSize int
	pack     DomainPagePack
	charge   uint64
}

// No entry handle escapes this table: a wire proof contains values only and
// cannot pin an evicted snapshot. All access uses domainPagePackDecodeSlot.
// Fixed storage avoids an uncharged, dynamically growing map/list allocation.
type decodedPackMemo struct {
	entries                         [decodedPackMemoMaximumEntries]decodedPackMemoEntry
	count                           int
	maximum, charged                uint64
	hits, misses, evicted, declined uint64
}

const decodedPackMemoBaseCharge = uint64(unsafe.Sizeof(decodedPackMemo{}))

var processDecodedPackMemo = newDecodedPackMemo(decodedPackMemoMaximumBytes)

func newDecodedPackMemo(maximum uint64) decodedPackMemo {
	if maximum > decodedPackMemoMaximumBytes {
		maximum = decodedPackMemoMaximumBytes
	}
	if maximum < decodedPackMemoBaseCharge {
		maximum = decodedPackMemoBaseCharge
	}
	return decodedPackMemo{maximum: maximum, charged: decodedPackMemoBaseCharge}
}

// DecodeDomainPagePackWithDecodedMemo is the opt-in provider-read equivalent of
// DecodeDomainPagePackWithWireProof. It owns the bounded wire inside admission,
// hashes all of it, and either strictly decodes or clones a private prior strict
// result for exactly those bytes and expected bindings. Only successful reads
// return an owned wire and opaque value proof. Callers must perform the actual
// provider GET independently; this function has no backend or presence state.
func DecodeDomainPagePackWithDecodedMemo(ctx context.Context, data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string) (DomainPagePack, []byte, DomainPagePackWireProof, error) {
	return materializeDecodedDomainPack(ctx, data, expectedDomainID, expectedKind, expectedPackID, nil, true, &processDecodedPackMemo)
}

// DecodeProvenDomainPagePackWithDecodedMemo keeps an existing cached wire proof
// fail-closed. A mismatched/zero proof is never treated as a memo miss. The wire
// is already caller-owned immutable data under the existing wire-cache contract.
func DecodeProvenDomainPagePackWithDecodedMemo(ctx context.Context, data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string, proof DomainPagePackWireProof) (DomainPagePack, error) {
	pack, _, _, err := materializeDecodedDomainPack(ctx, data, expectedDomainID, expectedKind, expectedPackID, &proof, false, &processDecodedPackMemo)
	return pack, err
}

// The memo parameter permits isolated unit instances, never a caller-supplied
// cache or validation callback. Every public entry passes the one bounded table.
func materializeDecodedDomainPack(ctx context.Context, data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string, proof *DomainPagePackWireProof, ownWire bool, memo *decodedPackMemo) (outcome DomainPagePack, owned []byte, issued DomainPagePackWireProof, err error) {
	release, err := admitDomainPagePackDecode(ctx)
	if err != nil {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, err
	}
	defer release()
	ctx, activity := telemetry.Start(ctx, telemetry.MetadataPackMaterialize, telemetry.State)
	defer telemetry.Finish(activity, &err)
	activity.Bytes(int64(len(data)), 0)
	if err := decodedMemoContextError(ctx); err != nil {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, err
	}
	if len(data) <= len(domainPagePackMagic) || len(data) > MaxDomainPagePackBytes || !bytes.Equal(data[:len(domainPagePackMagic)], domainPagePackMagic) {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, decodedMemoInvalid()
	}
	if ownWire {
		data = bytes.Clone(data)
	}
	key := decodedPackMemoKey{digest: sha256.Sum256(data), wireSize: len(data), domainID: expectedDomainID, kind: expectedKind, packID: expectedPackID}
	if err := decodedMemoContextError(ctx); err != nil {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, err
	}
	if proof != nil && (!proof.verified || proof.digest != key.digest || proof.size != key.wireSize || proof.domainID != key.domainID || proof.kind != key.kind || proof.packID != key.packID) {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, decodedMemoInvalid()
	}
	var pack DomainPagePack
	var prepared decodedPackMemoEntry
	var publish bool
	if index := memo.index(key); index >= 0 {
		memo.hits++
		pack, err = cloneDecodedDomainPack(ctx, memo.entries[index].pack)
		if err != nil {
			return DomainPagePack{}, nil, DomainPagePackWireProof{}, err
		}
		memo.promote(index)
	} else {
		memo.misses++
		// A miss preserves the existing strict/provider or proven-wire decoder.
		// Its bounded gzip, JSON, canonical/member guarantees remain intact.
		pack, err = decodeDomainPagePackAdmitted(ctx, data, expectedDomainID, expectedKind, expectedPackID, proof, nil)
		if err != nil {
			reason := telemetry.CorruptMetadata
			if errors.Is(err, context.Canceled) {
				reason = telemetry.ContextCanceled
			}
			if errors.Is(err, context.DeadlineExceeded) {
				reason = telemetry.ContextDeadline
			}
			return DomainPagePack{}, nil, DomainPagePackWireProof{}, telemetry.Diagnostic(err, telemetry.MetadataMaterialize, reason)
		}
		prepared, publish, err = memo.prepare(ctx, key, pack)
		if err != nil {
			return DomainPagePack{}, nil, DomainPagePackWireProof{}, err
		}
	}
	if err := decodedMemoContextError(ctx); err != nil {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, err
	}
	issued = DomainPagePackWireProof{verified: true, size: key.wireSize, digest: key.digest, domainID: expectedDomainID, kind: expectedKind, packID: expectedPackID}
	if ownWire {
		owned = data
	}
	// No fallible/cancellation boundary follows publication. A call that
	// returns an observed failure cannot admit its newly prepared snapshot.
	if publish {
		memo.publish(prepared)
	}
	return pack, owned, issued, nil
}

func decodedMemoInvalid() error {
	return telemetry.Diagnostic(domain.NewError(domain.ErrorInvalid, "consistency-domain decoded memo binding mismatch"), telemetry.MetadataMaterialize, telemetry.CorruptMetadata)
}

func decodedMemoContextError(ctx context.Context) error {
	if cause := ctx.Err(); cause != nil {
		reason := telemetry.ContextCanceled
		if errors.Is(cause, context.DeadlineExceeded) {
			reason = telemetry.ContextDeadline
		}
		return telemetry.Diagnostic(domain.WrapError(domain.ErrorUnavailable, "consistency-domain materialization canceled", cause), telemetry.MetadataMaterialize, reason)
	}
	return nil
}

func (memo *decodedPackMemo) index(key decodedPackMemoKey) int {
	for index := 0; index < memo.count; index++ {
		entry := &memo.entries[index]
		if entry.wireSize == key.wireSize && entry.digest == key.digest && entry.pack.DomainID == key.domainID && entry.pack.Kind == key.kind && entry.pack.PackID == key.packID {
			return index
		}
	}
	return -1
}

func (memo *decodedPackMemo) promote(index int) {
	entry := memo.entries[index]
	copy(memo.entries[1:index+1], memo.entries[:index])
	memo.entries[0] = entry
}

func (memo *decodedPackMemo) evictLast() {
	index := memo.count - 1
	memo.charged -= memo.entries[index].charge
	memo.entries[index] = decodedPackMemoEntry{}
	memo.count--
	memo.evicted++
}

func (memo *decodedPackMemo) prepare(ctx context.Context, key decodedPackMemoKey, pack DomainPagePack) (decodedPackMemoEntry, bool, error) {
	charge, err := decodedDomainPackCharge(ctx, pack)
	if err != nil {
		return decodedPackMemoEntry{}, false, err
	}
	if charge > memo.maximum-decodedPackMemoBaseCharge {
		memo.declined++
		return decodedPackMemoEntry{}, false, nil
	}
	// Evict before allocating the sealed copy, so both retained and under-
	// construction snapshot backing storage fit the same charged budget.
	for memo.count >= decodedPackMemoMaximumEntries || memo.charged+charge > memo.maximum {
		memo.evictLast()
	}
	sealed, err := cloneDecodedDomainPack(ctx, pack)
	if err != nil {
		return decodedPackMemoEntry{}, false, err
	}
	return decodedPackMemoEntry{digest: key.digest, wireSize: key.wireSize, pack: sealed, charge: charge}, true, nil
}

func (memo *decodedPackMemo) publish(entry decodedPackMemoEntry) {
	copy(memo.entries[1:memo.count+1], memo.entries[:memo.count])
	memo.entries[0] = entry
	memo.count++
	memo.charged += entry.charge
}

// This is an explicit retained-storage charge model, not a process-RSS claim.
// Fixed table/headers are charged once; each separately owned backing allocation
// is charged to a power of two above requested bytes plus 16 bytes of allowance.
// Strings are cloned too, so unknown external backing capacities cannot escape
// charging. Go allocator/GC metadata and caller-owned returned copies still need
// measured memory qualification under the actual pinned runtime and caps.
func decodedAllocationCharge(size uint64) uint64 {
	if size == 0 {
		return 0
	}
	if size > decodedPackMemoMaximumBytes {
		return decodedPackMemoMaximumBytes + 1
	}
	charge := uint64(32)
	for charge < size+16 {
		charge <<= 1
	}
	return charge
}

func decodedArrayCharge(count int, size uintptr) uint64 {
	if count < 0 || size == 0 {
		return decodedPackMemoMaximumBytes + 1
	}
	if uint64(count) > decodedPackMemoMaximumBytes/uint64(size) {
		return decodedPackMemoMaximumBytes + 1
	}
	return decodedAllocationCharge(uint64(count) * uint64(size))
}

func decodedDomainPackCharge(ctx context.Context, pack DomainPagePack) (uint64, error) {
	charge := decodedArrayCharge(len(pack.Pages), unsafe.Sizeof(DomainPackedPage{}))
	addString := func(value string) { charge += decodedAllocationCharge(uint64(len(value))) }
	addString(pack.DomainID)
	addString(string(pack.Kind))
	addString(pack.PackID)
	for _, packed := range pack.Pages {
		if err := decodedMemoContextError(ctx); err != nil {
			return 0, err
		}
		addString(packed.Digest)
		addString(packed.Page.DomainID)
		addString(string(packed.Page.Kind))
		charge += decodedArrayCharge(len(packed.Page.Entries), unsafe.Sizeof(DomainEntry{}))
		charge += decodedArrayCharge(len(packed.Page.Children), unsafe.Sizeof(DomainPageChild{}))
		for _, entry := range packed.Page.Entries {
			if err := decodedMemoContextError(ctx); err != nil {
				return 0, err
			}
			addString(entry.Key)
			addString(entry.LogicalVersion)
			charge += decodedAllocationCharge(uint64(len(entry.Value)))
		}
		for _, child := range packed.Page.Children {
			if err := decodedMemoContextError(ctx); err != nil {
				return 0, err
			}
			addString(child.FirstKey)
			addString(child.LastKey)
			addString(child.Digest)
			addString(child.PackID)
			addString(child.LeafKeyFilter)
		}
		if charge > decodedPackMemoMaximumBytes {
			return decodedPackMemoMaximumBytes + 1, nil
		}
	}
	return charge, decodedMemoContextError(ctx)
}

func cloneDecodedDomainPack(ctx context.Context, source DomainPagePack) (DomainPagePack, error) {
	if err := decodedMemoContextError(ctx); err != nil {
		return DomainPagePack{}, err
	}
	result := source
	result.DomainID = strings.Clone(source.DomainID)
	result.Kind = ConsistencyDomainKind(strings.Clone(string(source.Kind)))
	result.PackID = strings.Clone(source.PackID)
	if source.Pages != nil {
		result.Pages = make([]DomainPackedPage, len(source.Pages))
	}
	for index, packed := range source.Pages {
		if err := decodedMemoContextError(ctx); err != nil {
			return DomainPagePack{}, err
		}
		page := packed.Page
		page.DomainID = strings.Clone(page.DomainID)
		page.Kind = ConsistencyDomainKind(strings.Clone(string(page.Kind)))
		if page.Entries != nil {
			page.Entries = make([]DomainEntry, len(packed.Page.Entries))
		}
		for entryIndex, entry := range packed.Page.Entries {
			if err := decodedMemoContextError(ctx); err != nil {
				return DomainPagePack{}, err
			}
			value, err := cloneDecodedValue(ctx, entry.Value)
			if err != nil {
				return DomainPagePack{}, err
			}
			page.Entries[entryIndex] = DomainEntry{Key: strings.Clone(entry.Key), Value: value, LogicalVersion: strings.Clone(entry.LogicalVersion)}
		}
		if page.Children != nil {
			page.Children = make([]DomainPageChild, len(packed.Page.Children))
		}
		for childIndex, child := range packed.Page.Children {
			if err := decodedMemoContextError(ctx); err != nil {
				return DomainPagePack{}, err
			}
			child.FirstKey = strings.Clone(child.FirstKey)
			child.LastKey = strings.Clone(child.LastKey)
			child.Digest = strings.Clone(child.Digest)
			child.PackID = strings.Clone(child.PackID)
			child.LeafKeyFilter = strings.Clone(child.LeafKeyFilter)
			page.Children[childIndex] = child
		}
		result.Pages[index] = DomainPackedPage{Digest: strings.Clone(packed.Digest), Page: page}
	}
	return result, decodedMemoContextError(ctx)
}

func cloneDecodedValue(ctx context.Context, source []byte) ([]byte, error) {
	if source == nil {
		return nil, nil
	}
	result := make([]byte, len(source))
	const chunk = 64 << 10
	for offset := 0; offset < len(source); offset += chunk {
		if err := decodedMemoContextError(ctx); err != nil {
			return nil, err
		}
		end := min(offset+chunk, len(source))
		copy(result[offset:end], source[offset:end])
	}
	return result, decodedMemoContextError(ctx)
}
