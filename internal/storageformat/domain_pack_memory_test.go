package storageformat

import (
	"bytes"
	"fmt"
	"testing"
)

// Characterize valid metadata amplification before selecting a memory repair.
// Values model repetitive control records, not file contents. Both the wire
// and expanded format bounds remain those of the existing schema.
func metadataAmplificationPack(t testing.TB) (DomainPagePack, []byte) {
	t.Helper()
	pack := DomainPagePack{SchemaVersion: 1, DomainID: "memory-owner", Kind: DomainNamespace, PackID: Digest([]byte("memory-pack"))}
	for pageIndex := range 8 {
		page := DomainPage{SchemaVersion: 1, DomainID: pack.DomainID, Kind: pack.Kind}
		for entryIndex := range 16 {
			page.Entries = append(page.Entries, DomainEntry{
				Key:            fmt.Sprintf("metadata-%03d-%03d", pageIndex, entryIndex),
				LogicalVersion: Digest([]byte(fmt.Sprintf("version-%d-%d", pageIndex, entryIndex))),
				Value:          bytes.Repeat([]byte(`{"record":"retained-control-metadata"}`), 512),
			})
		}
		body, err := EncodeCanonical(page)
		if err != nil {
			t.Fatal(err)
		}
		pack.Pages = append(pack.Pages, DomainPackedPage{Digest: Digest(body), Page: page})
	}
	wire, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	return pack, wire
}

func BenchmarkDomainPagePackDecodeMetadataAmplification(b *testing.B) {
	pack, wire := metadataAmplificationPack(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		decoded, err := DecodeDomainPagePack(wire, pack.DomainID, pack.Kind, pack.PackID)
		if err != nil || len(decoded.Pages) != len(pack.Pages) {
			b.Fatalf("decode metadata fixture: %v", err)
		}
	}
	b.ReportMetric(float64(len(wire)), "wire-B")
}

// The selected decoder measured 37.9 MB/op on this valid metadata workload,
// versus 49.6 MB/op before ownership and duplicate-validation repairs. This
// ratchet comes from that comparison; it is not a guessed architecture ceiling.
func TestDomainPagePackDecodeRetainsMeasuredAllocationImprovement(t *testing.T) {
	pack, wire := metadataAmplificationPack(t)
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			decoded, err := DecodeDomainPagePack(wire, pack.DomainID, pack.Kind, pack.PackID)
			if err != nil || len(decoded.Pages) != len(pack.Pages) {
				b.Fatalf("decode: %v", err)
			}
		}
	})
	if allocated := result.AllocedBytesPerOp(); allocated > domainPackDecodeAllocationBudget {
		t.Fatalf("metadata pack decode allocated %d bytes/op, exceeds measured %s ratchet of %d bytes (wire %d bytes)", allocated, domainPackAllocationMode, domainPackDecodeAllocationBudget, len(wire))
	}
}
