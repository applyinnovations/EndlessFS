package storageformat

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
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

// EncodeDomainPagePack returns a deterministic gzip member prefixed by a
// format discriminator. Object bytes are canonical state metadata, never file
// content. Stable headers make retry equality and raw-copy portability exact.
func EncodeDomainPagePack(pack DomainPagePack) ([]byte, error) {
	pack.Pages = append([]DomainPackedPage(nil), pack.Pages...)
	sort.Slice(pack.Pages, func(i, j int) bool { return pack.Pages[i].Digest < pack.Pages[j].Digest })
	if err := ValidateDomainPagePack(pack); err != nil {
		return nil, err
	}
	var expanded bytes.Buffer
	encoder := json.NewEncoder(&expanded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(pack); err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "encode consistency-domain page pack", err)
	}
	canonical := bytes.TrimSuffix(expanded.Bytes(), []byte{'\n'})
	if len(canonical) == 0 || len(canonical) > MaxExpandedDomainPagePackBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "expanded consistency-domain page pack exceeds size limit")
	}
	compressed := encodeDeterministicGZIP(domainPagePackMagic, canonical)
	if len(compressed) > MaxDomainPagePackBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "consistency-domain page pack exceeds size limit")
	}
	return compressed, nil
}

func DecodeDomainPagePack(data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string) (DomainPagePack, error) {
	if len(data) <= len(domainPagePackMagic) || len(data) > MaxDomainPagePackBytes || !bytes.Equal(data[:len(domainPagePackMagic)], domainPagePackMagic) {
		return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain page pack envelope")
	}
	reader, err := gzip.NewReader(bytes.NewReader(data[len(domainPagePackMagic):]))
	if err != nil {
		return DomainPagePack{}, domain.WrapError(domain.ErrorInvalid, "open consistency-domain page pack", err)
	}
	expanded, readErr := io.ReadAll(io.LimitReader(reader, MaxExpandedDomainPagePackBytes+1))
	closeErr := reader.Close()
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
	if err := state.DecodeJSONWithLimit(expanded, &pack, MaxExpandedDomainPagePackBytes); err != nil {
		return DomainPagePack{}, err
	}
	if pack.DomainID != expectedDomainID || pack.Kind != expectedKind || pack.PackID != expectedPackID {
		return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "consistency-domain page pack key binding mismatch")
	}
	if err := ValidateDomainPagePack(pack); err != nil {
		return DomainPagePack{}, err
	}
	canonical, err := EncodeDomainPagePack(pack)
	if err != nil || !bytes.Equal(canonical, data) {
		return DomainPagePack{}, domain.NewError(domain.ErrorInvalid, "non-canonical consistency-domain page pack encoding")
	}
	return pack, nil
}
