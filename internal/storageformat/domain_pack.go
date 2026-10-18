package storageformat

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"io"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const (
	// MaxDomainPagePackBytes is the provider-wire envelope used by the schema-
	// 011 efficiency model. The expanded bound is independent and protects the
	// service from compression bombs while allowing highly repetitive canonical
	// page records to coalesce efficiently.
	MaxDomainPagePackBytes         = 4 << 20
	MaxExpandedDomainPagePackBytes = 32 << 20
)

var domainPagePackMagic = []byte("EFS-PACK-1\n")

// Expansion, JSON decoding and canonical verification can each temporarily
// hold the expanded metadata. Bound this work across every engine in a process,
// including unrelated packs that cannot share an in-flight read. Waiting must
// remain cancellable; it must not allocate an expanded buffer.
var domainPagePackDecodeSlot = make(chan struct{}, 1)

type domainPackContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader domainPackContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

// ErrDomainPagePackTooLarge distinguishes an encoding optimization's size
// bound from malformed authority. Writers may retry with canonical pages;
// readers must continue to reject an oversized stored pack.
var ErrDomainPagePackTooLarge = errors.New("consistency-domain page pack exceeds encoding bounds")

// EncodeDomainPagePack returns a deterministic gzip member prefixed by a
// format discriminator. Object bytes are canonical state metadata, never file
// content. Stable headers make retry equality and raw-copy portability exact.
func EncodeDomainPagePack(pack DomainPagePack) ([]byte, error) {
	pack.Pages = append([]DomainPackedPage(nil), pack.Pages...)
	sort.Slice(pack.Pages, func(i, j int) bool { return pack.Pages[i].Digest < pack.Pages[j].Digest })
	if err := ValidateDomainPagePack(pack); err != nil {
		return nil, err
	}
	return encodeValidatedDomainPagePack(pack)
}

// Both callers have already validated every page, including its digest and
// ordering. Canonical wire comparison must not validate and allocate all
// page encodings a second time.
func encodeValidatedDomainPagePack(pack DomainPagePack) ([]byte, error) {
	var expanded bytes.Buffer
	encoder := json.NewEncoder(&expanded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(pack); err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "encode consistency-domain page pack", err)
	}
	canonical := bytes.TrimSuffix(expanded.Bytes(), []byte{'\n'})
	if len(canonical) == 0 || len(canonical) > MaxExpandedDomainPagePackBytes {
		return nil, domain.WrapError(domain.ErrorInvalid, "expanded consistency-domain page pack exceeds size limit", ErrDomainPagePackTooLarge)
	}
	compressed := encodeDeterministicGZIP(domainPagePackMagic, canonical)
	if len(compressed) > MaxDomainPagePackBytes {
		return nil, domain.WrapError(domain.ErrorInvalid, "consistency-domain page pack exceeds size limit", ErrDomainPagePackTooLarge)
	}
	return compressed, nil
}

func DecodeDomainPagePack(ctx context.Context, data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string) (DomainPagePack, error) {
	return decodeDomainPagePack(ctx, data, expectedDomainID, expectedKind, expectedPackID, nil, nil)
}

func decodeDomainPagePack(ctx context.Context, data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string, proof *DomainPagePackWireProof, ownedWire *[]byte) (outcome DomainPagePack, err error) {
	if err := ctx.Err(); err != nil {
		return DomainPagePack{}, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack decode canceled", err)
	}
	select {
	case domainPagePackDecodeSlot <- struct{}{}:
		defer func() { <-domainPagePackDecodeSlot }()
	case <-ctx.Done():
		return DomainPagePack{}, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack decode canceled", ctx.Err())
	}
	_, activity := telemetry.Start(ctx, telemetry.MetadataPackDecode, telemetry.State)
	defer telemetry.Finish(activity, &err)
	activity.Bytes(int64(len(data)), 0)
	if len(data) <= len(domainPagePackMagic) || len(data) > MaxDomainPagePackBytes || !bytes.Equal(data[:len(domainPagePackMagic)], domainPagePackMagic) {
		return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain page pack envelope")
	}
	// An issuing call takes ownership only after admission and the wire bound,
	// so queued readers do not allocate an additional compressed-body copy.
	if ownedWire != nil {
		data = bytes.Clone(data)
	}
	// The memo can only be issued by a complete strict decode of these exact
	// owned bytes. Check it before expansion; a mismatch never falls back.
	if proof != nil && !proof.matches(data, expectedDomainID, expectedKind, expectedPackID) {
		return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "consistency-domain pack validation proof mismatch")
	}
	reader, err := gzip.NewReader(bytes.NewReader(data[len(domainPagePackMagic):]))
	if err != nil {
		return DomainPagePack{}, domain.WrapError(domain.ErrorInvalid, "open consistency-domain page pack", err)
	}
	expanded, readErr := io.ReadAll(io.LimitReader(domainPackContextReader{ctx: ctx, reader: reader}, MaxExpandedDomainPagePackBytes+1))
	activity.Bytes(0, int64(len(expanded)))
	closeErr := reader.Close()
	if err := ctx.Err(); err != nil {
		return DomainPagePack{}, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack decode canceled", err)
	}
	if readErr != nil || closeErr != nil || len(expanded) == 0 || len(expanded) > MaxExpandedDomainPagePackBytes {
		if readErr != nil {
			return DomainPagePack{}, domain.WrapError(domain.ErrorInvalid, "expand consistency-domain page pack", readErr)
		}
		if closeErr != nil {
			return DomainPagePack{}, domain.WrapError(domain.ErrorInvalid, "close consistency-domain page pack", closeErr)
		}
		return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "expanded consistency-domain page pack exceeds size limit")
	}
	var pack DomainPagePack
	// Exact canonical wire equality below rejects duplicate/unknown fields,
	// invalid UTF-8, trailing JSON, whitespace and alternate field spellings.
	// Unmarshal avoids two decoder-owned copies and the duplicate-token scan
	// of the complete expanded pack; no unvalidated page escapes this function.
	if err := json.Unmarshal(expanded, &pack); err != nil {
		return DomainPagePack{}, domain.WrapError(domain.ErrorInvalid, "invalid consistency-domain page pack JSON", err)
	}
	if err := ctx.Err(); err != nil {
		return DomainPagePack{}, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack decode canceled", err)
	}
	if pack.DomainID != expectedDomainID || pack.Kind != expectedKind || pack.PackID != expectedPackID {
		return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "consistency-domain page pack key binding mismatch")
	}
	if proof == nil {
		if err := ValidateDomainPagePack(pack); err != nil {
			return DomainPagePack{}, err
		}
		canonical, err := encodeValidatedDomainPagePack(pack)
		if err != nil || !bytes.Equal(canonical, data) {
			return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "non-canonical consistency-domain page pack encoding")
		}
	}
	// For proven wire, canonical spelling, member ordering, page validity and
	// digests are deterministic properties already established for these exact
	// SHA-256-bound bytes. Expansion, gzip integrity and fresh JSON parsing are
	// still performed, and callers retain their per-use descriptor checks.
	if err := ctx.Err(); err != nil {
		return DomainPagePack{}, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack decode canceled", err)
	}
	if ownedWire != nil {
		*ownedWire = data
	}
	return pack, nil
}
