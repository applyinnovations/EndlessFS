package storageformat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// Preserve the preceding buffer-based implementation as an independent wire
// oracle. A matching decoded value alone would miss changed compressed bytes,
// pack boundaries, retry equality and create-only object collisions.
func previousDomainPagePackEncoding(pack DomainPagePack) ([]byte, error) {
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
		return nil, domain.WrapError(domain.ErrorInvalid, "expanded consistency-domain page pack exceeds size limit", ErrDomainPagePackTooLarge)
	}
	compressed := encodeDeterministicGZIP(domainPagePackMagic, canonical)
	if len(compressed) > MaxDomainPagePackBytes {
		return nil, domain.WrapError(domain.ErrorInvalid, "consistency-domain page pack exceeds size limit", ErrDomainPagePackTooLarge)
	}
	return compressed, nil
}

func TestDomainPagePackEncoderMatchesPreviousWireAndValidatesUnusedMembers(t *testing.T) {
	empty := testDomainPagePackWithValues(t, [][]byte{nil, {}, []byte("<>&\"\u2028\u2029"), deterministicNoise(257, 11)})
	// The public encoder must retain its digest sorting, without modifying the
	// caller's page order or silently accepting invalid unused pages.
	sort.Slice(empty.Pages, func(i, j int) bool { return empty.Pages[i].Digest > empty.Pages[j].Digest })
	long := testDomainPagePackWithValues(t, [][]byte{bytes.Repeat([]byte("canonical <>& metadata"), 8192)})
	escaped := testDomainPagePack(t)
	escaped.DomainID = "owner<>&\"雪\u2028\u2029"
	page := &escaped.Pages[0].Page
	page.DomainID = escaped.DomainID
	page.Entries[0].Key = "key<>&\"雪\u2028\u2029"
	page.Entries[0].LogicalVersion = "version<>&\"雪\u2028\u2029"
	canonical, err := EncodeCanonical(*page)
	if err != nil {
		t.Fatal(err)
	}
	escaped.Pages[0].Digest = Digest(canonical)
	branch := testDomainPagePack(t)
	branchPage := DomainPage{
		SchemaVersion: 1, DomainID: branch.DomainID, Kind: branch.Kind, Level: 1,
		Entries:  []DomainEntry{},
		Children: []DomainPageChild{{FirstKey: "alpha", LastKey: "alpha", Digest: branch.Pages[0].Digest, PackID: branch.PackID, Level: 0, EntryCount: 1, ByteCount: uint64(len(branch.Pages[0].Page.Entries[0].Value))}},
	}
	canonical, err = EncodeCanonical(branchPage)
	if err != nil {
		t.Fatal(err)
	}
	branch.Pages = append(branch.Pages, DomainPackedPage{Digest: Digest(canonical), Page: branchPage})
	for name, pack := range map[string]DomainPagePack{
		"ordinary": testDomainPagePack(t), "unordered-nil-empty-opaque": empty,
		"long": long, "string-escaping": escaped, "branch-empty-entries": branch,
	} {
		t.Run(name, func(t *testing.T) {
			originalOrder := append([]DomainPackedPage(nil), pack.Pages...)
			want, err := previousDomainPagePackEncoding(pack)
			if err != nil {
				t.Fatal(err)
			}
			got, err := EncodeDomainPagePack(pack)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("wire differs from previous encoder: error=%v got=%d want=%d", err, len(got), len(want))
			}
			decoded, err := DecodeDomainPagePack(context.Background(), got, pack.DomainID, pack.Kind, pack.PackID)
			if err != nil || len(decoded.Pages) != len(pack.Pages) {
				t.Fatalf("strict decode: pages=%d error=%v", len(decoded.Pages), err)
			}
			for index, page := range pack.Pages {
				if page.Digest != originalOrder[index].Digest {
					t.Fatal("encoder changed caller page order")
				}
			}
		})
	}
	for name, mutate := range map[string]func(*DomainPagePack){
		"unused-digest": func(pack *DomainPagePack) { pack.Pages[1].Digest = Digest([]byte("wrong digest")) },
		"unused-domain": func(pack *DomainPagePack) { pack.Pages[1].Page.DomainID = "another-owner" },
		"unused-schema": func(pack *DomainPagePack) { pack.Pages[1].Page.SchemaVersion = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			pack := testDomainPagePackWithValues(t, [][]byte{[]byte("used"), []byte("unused")})
			mutate(&pack)
			got, err := EncodeDomainPagePack(pack)
			if !errors.Is(err, domain.ErrInvalid) || got != nil {
				t.Fatalf("invalid unused member escaped: bytes=%d error=%v", len(got), err)
			}
		})
	}
}
