package storageformat

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func testDomainPagePack(t *testing.T) DomainPagePack {
	t.Helper()
	page := DomainPage{
		SchemaVersion: 1,
		DomainID:      "owner-a",
		Kind:          DomainNamespace,
		Level:         0,
		Entries: []DomainEntry{{
			Key: "alpha", Value: []byte(`{"value":1}`), LogicalVersion: "version-a",
		}},
	}
	body, err := EncodeCanonical(page)
	if err != nil {
		t.Fatal(err)
	}
	return DomainPagePack{
		SchemaVersion: 1,
		DomainID:      page.DomainID,
		Kind:          page.Kind,
		PackID:        Digest([]byte("pack-a")),
		Pages:         []DomainPackedPage{{Digest: Digest(body), Page: page}},
	}
}

func testDomainPagePackWithValues(t *testing.T, values [][]byte) DomainPagePack {
	t.Helper()
	pack := DomainPagePack{
		SchemaVersion: 1, DomainID: "owner-a", Kind: DomainNamespace,
		PackID: Digest([]byte(fmt.Sprintf("large-pack-%d", len(values)))),
		Pages:  make([]DomainPackedPage, len(values)),
	}
	for index, value := range values {
		page := DomainPage{
			SchemaVersion: 1, DomainID: pack.DomainID, Kind: pack.Kind, Level: 0,
			Entries: []DomainEntry{{Key: fmt.Sprintf("entry-%03d", index), Value: value, LogicalVersion: "version"}},
		}
		body, err := EncodeCanonical(page)
		if err != nil {
			t.Fatal(err)
		}
		pack.Pages[index] = DomainPackedPage{Digest: Digest(body), Page: page}
	}
	return pack
}

func deterministicNoise(size int, seed uint64) []byte {
	value := make([]byte, size)
	state := seed
	for index := range value {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		value[index] = byte(state)
	}
	return value
}

func encodeDomainPackPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	encoded.Write(domainPagePackMagic)
	writer, err := gzip.NewWriterLevel(&encoded, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.OS = 255
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestDomainPagePackCanonicalRoundTripAndFailClosedEnvelope(t *testing.T) {
	pack := testDomainPagePack(t)
	body, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeDomainPagePack(pack)
	if err != nil || !bytes.Equal(body, second) {
		t.Fatalf("deterministic encoding = %v, equal=%v", err, bytes.Equal(body, second))
	}
	decoded, err := DecodeDomainPagePack(body, pack.DomainID, pack.Kind, pack.PackID)
	if err != nil || len(decoded.Pages) != 1 || decoded.Pages[0].Digest != pack.Pages[0].Digest {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}

	invalidPayload := encodeDomainPackPayload(t, []byte("["))
	nonCanonicalPayload, err := EncodeCanonical(pack)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical := encodeDomainPackPayload(t, append([]byte(" "), nonCanonicalPayload...))
	oversized := encodeDomainPackPayload(t, bytes.Repeat([]byte("x"), MaxExpandedDomainPagePackBytes+1))
	for name, candidate := range map[string][]byte{
		"empty":          nil,
		"magic-only":     append([]byte(nil), domainPagePackMagic...),
		"wrong-magic":    append([]byte("wrong-pack\n"), body[len(domainPagePackMagic):]...),
		"truncated-gzip": body[:len(body)-1],
		"invalid-json":   invalidPayload,
		"non-canonical":  nonCanonical,
		"expanded-limit": oversized,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDomainPagePack(candidate, pack.DomainID, pack.Kind, pack.PackID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
	for name, binding := range map[string]struct {
		domainID string
		kind     ConsistencyDomainKind
		packID   string
	}{
		"domain": {domainID: "owner-b", kind: pack.Kind, packID: pack.PackID},
		"kind":   {domainID: pack.DomainID, kind: DomainAdmin, packID: pack.PackID},
		"pack":   {domainID: pack.DomainID, kind: pack.Kind, packID: Digest([]byte("pack-b"))},
	} {
		t.Run("binding-"+name, func(t *testing.T) {
			if _, err := DecodeDomainPagePack(body, binding.domainID, binding.kind, binding.packID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
}

func TestDomainPagePackRejectsInvalidAndAmbiguousMembers(t *testing.T) {
	valid := testDomainPagePack(t)
	for name, mutate := range map[string]func(*DomainPagePack){
		"schema": func(pack *DomainPagePack) { pack.SchemaVersion = 0 },
		"domain": func(pack *DomainPagePack) { pack.DomainID = "" },
		"kind":   func(pack *DomainPagePack) { pack.Kind = "unknown" },
		"pack":   func(pack *DomainPagePack) { pack.PackID = "invalid" },
		"empty":  func(pack *DomainPagePack) { pack.Pages = nil },
		"digest": func(pack *DomainPagePack) { pack.Pages[0].Digest = Digest([]byte("wrong")) },
		"page-domain": func(pack *DomainPagePack) {
			pack.Pages[0].Page.DomainID = "owner-b"
		},
		"page-kind": func(pack *DomainPagePack) { pack.Pages[0].Page.Kind = DomainAdmin },
		"duplicate": func(pack *DomainPagePack) {
			pack.Pages = append(pack.Pages, pack.Pages[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Pages = append([]DomainPackedPage(nil), valid.Pages...)
			mutate(&candidate)
			if _, err := EncodeDomainPagePack(candidate); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
}

func TestDomainPagePackEnforcesIndependentExpandedAndWireBounds(t *testing.T) {
	expandedValues := make([][]byte, 43)
	for index := range expandedValues {
		expandedValues[index] = bytes.Repeat([]byte{byte('a' + index%26)}, 600_000)
	}
	if _, err := EncodeDomainPagePack(testDomainPagePackWithValues(t, expandedValues)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("expanded limit error = %v", err)
	}

	compressedValues := make([][]byte, 8)
	for index := range compressedValues {
		compressedValues[index] = deterministicNoise(600_000, uint64(index+1))
	}
	if _, err := EncodeDomainPagePack(testDomainPagePackWithValues(t, compressedValues)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("wire limit error = %v", err)
	}
}

func TestDomainPagePackRejectsInvalidCompressedAndCanonicalMembers(t *testing.T) {
	pack := testDomainPagePack(t)
	invalid := pack
	invalid.Pages = append([]DomainPackedPage(nil), pack.Pages...)
	invalid.Pages[0].Page.SchemaVersion = 0
	payload, err := EncodeCanonical(invalid)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"gzip":   append(append([]byte(nil), domainPagePackMagic...), []byte("not-gzip")...),
		"member": encodeDomainPackPayload(t, payload),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDomainPagePack(body, pack.DomainID, pack.Kind, pack.PackID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDomainLeafFilterAndBranchDescriptorRejectInvalidEncoding(t *testing.T) {
	if possible, err := DomainLeafKeyFilterMayContain("invalid", "key"); err == nil || possible {
		t.Fatalf("invalid filter result = %v, %v", possible, err)
	}
	page := DomainPage{
		SchemaVersion: 1, DomainID: "owner-a", Kind: DomainNamespace, Level: 1,
		Children: []DomainPageChild{{
			FirstKey: "a", LastKey: "z", Digest: Digest([]byte("leaf")), Level: 0,
			EntryCount: 1, LeafKeyFilter: "invalid",
		}},
	}
	body, err := EncodeCanonical(page)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDomainPage(page, Digest(body)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid branch filter error = %v", err)
	}
}
