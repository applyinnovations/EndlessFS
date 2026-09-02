package storageformat

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	// The terminal transient segment is a cumulative cancellation index for
	// the complete 10,000-item batch. It is deliberately outside canonical
	// checkpoint authority and may therefore use a larger bounded envelope than
	// application records. One read can recover every sealed provider lease
	// after a pod restart without one state GET per 1,000 sessions.
	MaxUploadLeaseSegmentBytes         = 32 << 20
	MaxExpandedUploadLeaseSegmentBytes = 32 << 20
)

var uploadLeaseSegmentMagic = []byte("EFS-LEASES-1\n")

// PortableUploadLeaseSegment is transient coordination state. Provider-native
// leases are sealed by the adapter, bounded here, and deliberately excluded
// from authoritative checkpoints. Consecutive 1,000-item segments cap the
// provider work that can be orphaned by a crash without adding a write per
// object.
type PortableUploadLeaseSegment struct {
	SchemaVersion int                   `json:"schemaVersion"`
	BackendKind   string                `json:"backendKind"`
	OwnerID       string                `json:"ownerID"`
	BatchID       string                `json:"batchID"`
	Segment       uint64                `json:"segment"`
	TotalCount    uint64                `json:"totalCount"`
	FirstIndex    uint64                `json:"firstIndex"`
	Leases        []PortableUploadLease `json:"leases"`
}

type PortableUploadLease struct {
	Index    uint64 `json:"index"`
	UploadID string `json:"uploadID"`
	Lease    []byte `json:"lease"`
}

func ValidatePortableUploadLeaseSegment(value PortableUploadLeaseSegment) error {
	if value.SchemaVersion != 1 || ValidateNamespace(value.BackendKind) != nil || !validDomainText(value.OwnerID) || !validDomainDigest(value.BatchID) || value.TotalCount < 1 || value.TotalCount > MaxPortableUploadBatchItems || value.Segment > (value.TotalCount-1)/MaxUploadLeaseSegmentItems || len(value.Leases) < 1 {
		return domain.NewError(domain.ErrorInvalid, "invalid upload lease segment")
	}
	terminal := uint64(len(value.Leases)) == value.TotalCount
	if terminal {
		if value.Segment != (value.TotalCount-1)/MaxUploadLeaseSegmentItems || value.FirstIndex != 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid cumulative upload lease segment")
		}
	} else if value.Segment >= (value.TotalCount-1)/MaxUploadLeaseSegmentItems || value.FirstIndex != value.Segment*MaxUploadLeaseSegmentItems || len(value.Leases) != MaxUploadLeaseSegmentItems {
		return domain.NewError(domain.ErrorInvalid, "invalid partial upload lease segment")
	}
	for offset, lease := range value.Leases {
		index := value.FirstIndex + uint64(offset)
		if lease.Index != index || lease.Index >= MaxPortableUploadBatchItems || !validDomainText(lease.UploadID) || len(lease.Lease) < 1 || len(lease.Lease) > MaxSealedUploadLeaseBytes {
			return domain.NewError(domain.ErrorInvalid, "invalid upload lease segment member")
		}
	}
	return nil
}

func EncodePortableUploadLeaseSegment(value PortableUploadLeaseSegment) ([]byte, error) {
	if err := ValidatePortableUploadLeaseSegment(value); err != nil {
		return nil, err
	}
	var expanded bytes.Buffer
	encoder := json.NewEncoder(&expanded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "encode upload lease segment", err)
	}
	canonical := bytes.TrimSuffix(expanded.Bytes(), []byte{'\n'})
	if len(canonical) == 0 || len(canonical) > MaxExpandedUploadLeaseSegmentBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "expanded upload lease segment exceeds size limit")
	}
	var compressed bytes.Buffer
	compressed.Write(uploadLeaseSegmentMagic)
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInternal, "initialize upload lease segment compression", err)
	}
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.OS = 255
	if _, err := writer.Write(canonical); err != nil {
		_ = writer.Close()
		return nil, domain.WrapError(domain.ErrorInternal, "compress upload lease segment", err)
	}
	if err := writer.Close(); err != nil {
		return nil, domain.WrapError(domain.ErrorInternal, "finish upload lease segment compression", err)
	}
	if compressed.Len() > MaxUploadLeaseSegmentBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "upload lease segment exceeds size limit")
	}
	return append([]byte(nil), compressed.Bytes()...), nil
}

func DecodePortableUploadLeaseSegment(data []byte, backendKind, ownerID, batchID string, segment uint64) (PortableUploadLeaseSegment, error) {
	if len(data) <= len(uploadLeaseSegmentMagic) || len(data) > MaxUploadLeaseSegmentBytes || !bytes.Equal(data[:len(uploadLeaseSegmentMagic)], uploadLeaseSegmentMagic) {
		return PortableUploadLeaseSegment{}, domain.NewError(domain.ErrorInvalid, "invalid upload lease segment envelope")
	}
	reader, err := gzip.NewReader(bytes.NewReader(data[len(uploadLeaseSegmentMagic):]))
	if err != nil {
		return PortableUploadLeaseSegment{}, domain.WrapError(domain.ErrorInvalid, "open upload lease segment", err)
	}
	expanded, readErr := io.ReadAll(io.LimitReader(reader, MaxExpandedUploadLeaseSegmentBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(expanded) == 0 || len(expanded) > MaxExpandedUploadLeaseSegmentBytes {
		return PortableUploadLeaseSegment{}, domain.NewError(domain.ErrorInvalid, "expand upload lease segment")
	}
	var value PortableUploadLeaseSegment
	if err := state.DecodeJSONWithLimit(expanded, &value, MaxExpandedUploadLeaseSegmentBytes); err != nil {
		return PortableUploadLeaseSegment{}, err
	}
	if value.BackendKind != backendKind || value.OwnerID != ownerID || value.BatchID != batchID || value.Segment != segment {
		return PortableUploadLeaseSegment{}, domain.NewError(domain.ErrorInvalid, "upload lease segment key binding mismatch")
	}
	if err := ValidatePortableUploadLeaseSegment(value); err != nil {
		return PortableUploadLeaseSegment{}, err
	}
	canonical, err := EncodePortableUploadLeaseSegment(value)
	if err != nil || !bytes.Equal(canonical, data) {
		return PortableUploadLeaseSegment{}, domain.NewError(domain.ErrorInvalid, "non-canonical upload lease segment encoding")
	}
	return value, nil
}
