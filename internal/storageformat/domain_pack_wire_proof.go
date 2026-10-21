package storageformat

import (
	"context"
	"crypto/sha256"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// DomainPagePackWireProof is an opaque, process-local memo of complete strict
// validation. It is not serializable authority or proof of provider presence.
// Its zero value is invalid; only a successful strict read can issue one.
type DomainPagePackWireProof struct {
	verified bool
	size     int
	digest   [sha256.Size]byte
	domainID string
	kind     ConsistencyDomainKind
	packID   string
}

// DecodeDomainPagePackWithWireProof owns a copy before validating and hashing,
// so a provider/caller cannot change the bytes between those operations. The
// returned wire belongs to the caller and must be kept immutable while cached.
// Failure returns neither owned wire nor a usable proof.
func DecodeDomainPagePackWithWireProof(ctx context.Context, data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string) (DomainPagePack, []byte, DomainPagePackWireProof, error) {
	var owned []byte
	// The shared decoder bounds/admit work before copying, then strictly
	// validates the same exclusively owned bytes that will be hashed below.
	pack, err := decodeDomainPagePack(ctx, data, expectedDomainID, expectedKind, expectedPackID, nil, &owned)
	if err != nil {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, err
	}
	digest := sha256.Sum256(owned)
	if err := ctx.Err(); err != nil {
		return DomainPagePack{}, nil, DomainPagePackWireProof{}, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack validation proof canceled", err)
	}
	proof := DomainPagePackWireProof{verified: true, size: len(owned), digest: digest, domainID: expectedDomainID, kind: expectedKind, packID: expectedPackID}
	return pack, owned, proof, nil
}

// DecodeProvenDomainPagePack still uses the cancellable single expansion slot,
// bounded gzip validation and fresh JSON-owned maps. The opaque memo permits
// reuse only of deterministic canonical/member verification for exact wire.
func DecodeProvenDomainPagePack(ctx context.Context, data []byte, expectedDomainID string, expectedKind ConsistencyDomainKind, expectedPackID string, proof DomainPagePackWireProof) (DomainPagePack, error) {
	return decodeDomainPagePack(ctx, data, expectedDomainID, expectedKind, expectedPackID, &proof, nil)
}

func (proof DomainPagePackWireProof) matches(data []byte, domainID string, kind ConsistencyDomainKind, packID string) bool {
	return proof.verified && proof.size == len(data) && proof.domainID == domainID && proof.kind == kind && proof.packID == packID && proof.digest == sha256.Sum256(data)
}
