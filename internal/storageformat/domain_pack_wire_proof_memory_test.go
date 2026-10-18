package storageformat

import (
	"context"
	"testing"
)

// Compare the same valid metadata fixture with the strict path. Setup and the
// first complete verification are outside this warm immutable-reuse observation.
func BenchmarkDomainPagePackDecodeProvenMetadataAmplification(b *testing.B) {
	pack, wire := metadataAmplificationPack(b)
	_, owned, proof, err := DecodeDomainPagePackWithWireProof(context.Background(), wire, pack.DomainID, pack.Kind, pack.PackID)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		decoded, err := DecodeProvenDomainPagePack(context.Background(), owned, pack.DomainID, pack.Kind, pack.PackID, proof)
		if err != nil || len(decoded.Pages) != len(pack.Pages) {
			b.Fatalf("decode proven metadata fixture: %v", err)
		}
	}
	b.ReportMetric(float64(len(owned)), "wire-B")
}

// Keep the measured warm-path improvement independently of the unchanged
// strict-decoder guard. Proof issuance is outside the repeated-hit boundary.
func TestDomainPagePackProvenDecodeRetainsMeasuredAllocationImprovement(t *testing.T) {
	pack, wire := metadataAmplificationPack(t)
	_, owned, proof, err := DecodeDomainPagePackWithWireProof(context.Background(), wire, pack.DomainID, pack.Kind, pack.PackID)
	if err != nil {
		t.Fatal(err)
	}
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			decoded, err := DecodeProvenDomainPagePack(context.Background(), owned, pack.DomainID, pack.Kind, pack.PackID, proof)
			if err != nil || len(decoded.Pages) != len(pack.Pages) {
				b.Fatalf("decode proven metadata fixture: %v", err)
			}
		}
	})
	if allocated := result.AllocedBytesPerOp(); allocated > domainPackProvenDecodeAllocationBudget {
		t.Fatalf("proven metadata pack decode allocated %d bytes/op, exceeds measured %s ratchet of %d bytes (wire %d bytes)", allocated, domainPackAllocationMode, domainPackProvenDecodeAllocationBudget, len(owned))
	}
}
