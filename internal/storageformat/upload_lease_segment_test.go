package storageformat

import (
	"bytes"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func validUploadLeaseSegment() PortableUploadLeaseSegment {
	batchID := Digest([]byte("batch"))
	return PortableUploadLeaseSegment{
		SchemaVersion: 1, BackendKind: "memory", OwnerID: "owner", BatchID: batchID,
		Segment: 0, TotalCount: 1, FirstIndex: 0,
		Leases: []PortableUploadLease{{Index: 0, UploadID: "upload", Lease: []byte("sealed")}},
	}
}

func TestPortableUploadLeaseSegmentCanonicalRoundTripAndBindings(t *testing.T) {
	value := validUploadLeaseSegment()
	body, err := EncodePortableUploadLeaseSegment(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePortableUploadLeaseSegment(body, value.BackendKind, value.OwnerID, value.BatchID, value.Segment)
	if err != nil || !bytes.Equal(decoded.Leases[0].Lease, value.Leases[0].Lease) {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	for name, decode := range map[string]func() error{
		"truncated": func() error {
			_, err := DecodePortableUploadLeaseSegment(body[:len(body)-1], value.BackendKind, value.OwnerID, value.BatchID, value.Segment)
			return err
		},
		"owner": func() error {
			_, err := DecodePortableUploadLeaseSegment(body, value.BackendKind, "other", value.BatchID, value.Segment)
			return err
		},
		"batch": func() error {
			_, err := DecodePortableUploadLeaseSegment(body, value.BackendKind, value.OwnerID, Digest([]byte("other")), value.Segment)
			return err
		},
		"segment": func() error {
			_, err := DecodePortableUploadLeaseSegment(body, value.BackendKind, value.OwnerID, value.BatchID, 2)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := decode(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
}

func TestPortableUploadLeaseSegmentRejectsEveryStructuralBoundary(t *testing.T) {
	valid := validUploadLeaseSegment()
	for name, mutate := range map[string]func(*PortableUploadLeaseSegment){
		"schema":      func(value *PortableUploadLeaseSegment) { value.SchemaVersion = 0 },
		"backend":     func(value *PortableUploadLeaseSegment) { value.BackendKind = "INVALID" },
		"owner":       func(value *PortableUploadLeaseSegment) { value.OwnerID = "" },
		"batch":       func(value *PortableUploadLeaseSegment) { value.BatchID = "bad" },
		"segment":     func(value *PortableUploadLeaseSegment) { value.Segment = 10 },
		"total-zero":  func(value *PortableUploadLeaseSegment) { value.TotalCount = 0 },
		"total-large": func(value *PortableUploadLeaseSegment) { value.TotalCount = MaxPortableUploadBatchItems + 1 },
		"first":       func(value *PortableUploadLeaseSegment) { value.FirstIndex++ },
		"empty":       func(value *PortableUploadLeaseSegment) { value.Leases = nil },
		"index":       func(value *PortableUploadLeaseSegment) { value.Leases[0].Index++ },
		"upload":      func(value *PortableUploadLeaseSegment) { value.Leases[0].UploadID = "" },
		"lease-empty": func(value *PortableUploadLeaseSegment) { value.Leases[0].Lease = nil },
		"lease-large": func(value *PortableUploadLeaseSegment) {
			value.Leases[0].Lease = make([]byte, MaxSealedUploadLeaseBytes+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Leases = append([]PortableUploadLease(nil), valid.Leases...)
			candidate.Leases[0].Lease = append([]byte(nil), valid.Leases[0].Lease...)
			mutate(&candidate)
			if _, err := EncodePortableUploadLeaseSegment(candidate); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
}

func TestPortableUploadLeaseSegmentsUseBoundedProgressAndOneTerminalCancellationIndex(t *testing.T) {
	const count = 1_500
	leasing := make([]PortableUploadLease, count)
	for index := range leasing {
		leasing[index] = PortableUploadLease{Index: uint64(index), UploadID: "upload-" + Digest([]byte{byte(index), byte(index >> 8)}), Lease: []byte("sealed-lease")}
	}
	base := validUploadLeaseSegment()
	partial := base
	partial.TotalCount = count
	partial.Leases = append([]PortableUploadLease(nil), leasing[:MaxUploadLeaseSegmentItems]...)
	if _, err := EncodePortableUploadLeaseSegment(partial); err != nil {
		t.Fatalf("encode bounded progress segment: %v", err)
	}
	terminal := base
	terminal.Segment = 1
	terminal.TotalCount = count
	terminal.Leases = append([]PortableUploadLease(nil), leasing...)
	body, err := EncodePortableUploadLeaseSegment(terminal)
	if err != nil {
		t.Fatalf("encode cumulative cancellation segment: %v", err)
	}
	decoded, err := DecodePortableUploadLeaseSegment(body, terminal.BackendKind, terminal.OwnerID, terminal.BatchID, terminal.Segment)
	if err != nil || len(decoded.Leases) != count || decoded.Leases[count-1].Index != count-1 {
		t.Fatalf("terminal cancellation index = %d leases, %v", len(decoded.Leases), err)
	}
}
