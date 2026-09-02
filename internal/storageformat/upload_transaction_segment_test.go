package storageformat

import (
	"bytes"
	"compress/gzip"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
)

func encodeUploadTransactionPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	encoded.Write(uploadTransactionSegmentMagic)
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

func validUploadTransactionSegment() UploadTransactionSegment {
	return UploadTransactionSegment{
		SchemaVersion: 1, BackendKind: "memory", OwnerID: "owner",
		TransactionID: Digest([]byte("transaction")), RequestFingerprint: Digest([]byte("request")),
		Kind: "complete", Segment: 1, FirstIndex: 0,
		Items: []UploadTransactionSegmentItem{{Index: 0, UploadID: "upload", MD5: integrity.MD5([]byte("body")), CRC32C: integrity.CRC32C([]byte("body"))}},
	}
}

func TestUploadTransactionSegmentCanonicalRoundTripAndBindings(t *testing.T) {
	value := validUploadTransactionSegment()
	body, err := EncodeUploadTransactionSegment(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUploadTransactionSegment(body, value.BackendKind, value.OwnerID, value.TransactionID, value.RequestFingerprint, value.Kind, value.Segment)
	if err != nil || decoded.Items[0] != value.Items[0] {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	for name, args := range map[string][]string{
		"backend":     {"other", value.OwnerID, value.TransactionID, value.RequestFingerprint, value.Kind},
		"owner":       {value.BackendKind, "other", value.TransactionID, value.RequestFingerprint, value.Kind},
		"transaction": {value.BackendKind, value.OwnerID, Digest([]byte("other")), value.RequestFingerprint, value.Kind},
		"fingerprint": {value.BackendKind, value.OwnerID, value.TransactionID, Digest([]byte("other")), value.Kind},
		"kind":        {value.BackendKind, value.OwnerID, value.TransactionID, value.RequestFingerprint, "abort"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeUploadTransactionSegment(body, args[0], args[1], args[2], args[3], args[4], value.Segment); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
	if _, err := DecodeUploadTransactionSegment(body[:len(body)-1], value.BackendKind, value.OwnerID, value.TransactionID, value.RequestFingerprint, value.Kind, value.Segment); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("truncated error = %v", err)
	}
	if canonical, _ := EncodeUploadTransactionSegment(decoded); !bytes.Equal(canonical, body) {
		t.Fatal("encoding is not deterministic")
	}
}

func TestUploadTransactionSegmentDecodeRejectsEveryEnvelopeBoundary(t *testing.T) {
	value := validUploadTransactionSegment()
	body, err := EncodeUploadTransactionSegment(value)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := EncodeCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	invalidValue := value
	invalidValue.SchemaVersion = 0
	invalidCanonical, err := EncodeCanonical(invalidValue)
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string][]byte{
		"empty":              nil,
		"magic-only":         append([]byte(nil), uploadTransactionSegmentMagic...),
		"invalid-gzip":       append(append([]byte(nil), uploadTransactionSegmentMagic...), []byte("not-gzip")...),
		"empty-expanded":     encodeUploadTransactionPayload(t, nil),
		"invalid-json":       encodeUploadTransactionPayload(t, []byte("[")),
		"invalid-record":     encodeUploadTransactionPayload(t, invalidCanonical),
		"non-canonical-json": encodeUploadTransactionPayload(t, append([]byte(" "), canonical...)),
		"truncated":          body[:len(body)-2],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeUploadTransactionSegment(candidate, value.BackendKind, value.OwnerID, value.TransactionID, value.RequestFingerprint, value.Kind, value.Segment); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
}

func TestUploadTransactionSegmentRejectsEveryStructuralBoundary(t *testing.T) {
	valid := validUploadTransactionSegment()
	for name, mutate := range map[string]func(*UploadTransactionSegment){
		"schema":      func(value *UploadTransactionSegment) { value.SchemaVersion = 0 },
		"backend":     func(value *UploadTransactionSegment) { value.BackendKind = "INVALID" },
		"owner":       func(value *UploadTransactionSegment) { value.OwnerID = "" },
		"transaction": func(value *UploadTransactionSegment) { value.TransactionID = "bad" },
		"fingerprint": func(value *UploadTransactionSegment) { value.RequestFingerprint = "bad" },
		"kind":        func(value *UploadTransactionSegment) { value.Kind = "other" },
		"segment":     func(value *UploadTransactionSegment) { value.Segment = 11 },
		"first":       func(value *UploadTransactionSegment) { value.FirstIndex++ },
		"empty":       func(value *UploadTransactionSegment) { value.Items = nil },
		"index":       func(value *UploadTransactionSegment) { value.Items[0].Index++ },
		"upload":      func(value *UploadTransactionSegment) { value.Items[0].UploadID = "" },
		"md5":         func(value *UploadTransactionSegment) { value.Items[0].MD5 = "bad" },
		"crc32c":      func(value *UploadTransactionSegment) { value.Items[0].CRC32C = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Items = append([]UploadTransactionSegmentItem(nil), valid.Items...)
			mutate(&candidate)
			if _, err := EncodeUploadTransactionSegment(candidate); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
	abort := valid
	abort.Kind = "abort"
	abort.Items = []UploadTransactionSegmentItem{{Index: 0, UploadID: "upload"}}
	if _, err := EncodeUploadTransactionSegment(abort); err != nil {
		t.Fatalf("valid abort = %v", err)
	}
	abort.Items[0].CRC32C = integrity.CRC32C(nil)
	if _, err := EncodeUploadTransactionSegment(abort); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("abort fingerprint = %v", err)
	}
}
