package storageformat

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	MaxUploadTransactionSegmentBytes         = 4 << 20
	MaxExpandedUploadTransactionSegmentBytes = 8 << 20
	UploadTransactionSegmentItems            = 1_000
)

var uploadTransactionSegmentMagic = []byte("EFS-UPLOAD-TXN-1\n")

// UploadTransactionSegment is transient crash progress. It contains only
// provider-attested content identity, never a capability, provider version,
// upload URL, or object body. The authoritative namespace head is the sole
// visibility point; an unreferenced segment is disposable.
type UploadTransactionSegment struct {
	SchemaVersion      int                            `json:"schemaVersion"`
	BackendKind        string                         `json:"backendKind"`
	OwnerID            string                         `json:"ownerID"`
	TransactionID      string                         `json:"transactionID"`
	RequestFingerprint string                         `json:"requestFingerprint"`
	Kind               string                         `json:"kind"`
	Segment            uint64                         `json:"segment"`
	FirstIndex         uint64                         `json:"firstIndex"`
	Items              []UploadTransactionSegmentItem `json:"items"`
}

type UploadTransactionSegmentItem struct {
	Index    uint64 `json:"index"`
	UploadID string `json:"uploadID"`
	MD5      string `json:"md5,omitempty"`
	CRC32C   string `json:"crc32c,omitempty"`
}

func ValidateUploadTransactionSegment(value UploadTransactionSegment) error {
	if value.SchemaVersion != 1 || ValidateNamespace(value.BackendKind) != nil || !validDomainText(value.OwnerID) || !validDomainDigest(value.TransactionID) || !validDomainDigest(value.RequestFingerprint) || value.Kind != "complete" && value.Kind != "abort" || value.Segment < 1 || value.Segment > MaxPortableUploadBatchItems/UploadTransactionSegmentItems || value.FirstIndex != 0 || len(value.Items) < 1 || len(value.Items) > MaxPortableUploadBatchItems || len(value.Items) > int(value.Segment)*UploadTransactionSegmentItems {
		return domain.NewError(domain.ErrorInvalid, "invalid upload transaction segment")
	}
	for offset, item := range value.Items {
		if item.Index != uint64(offset) || item.Index >= MaxPortableUploadBatchItems || !validDomainText(item.UploadID) {
			return domain.NewError(domain.ErrorInvalid, "invalid upload transaction segment member")
		}
		if value.Kind == "complete" {
			if _, ok := integrity.ParseMD5(item.MD5); !ok {
				return domain.NewError(domain.ErrorInvalid, "invalid upload transaction md5")
			}
			if _, ok := integrity.ParseCRC32C(item.CRC32C); !ok {
				return domain.NewError(domain.ErrorInvalid, "invalid upload transaction crc32c")
			}
		} else if item.MD5 != "" || item.CRC32C != "" {
			return domain.NewError(domain.ErrorInvalid, "abort transaction contains a fingerprint")
		}
	}
	return nil
}

func EncodeUploadTransactionSegment(value UploadTransactionSegment) ([]byte, error) {
	if err := ValidateUploadTransactionSegment(value); err != nil {
		return nil, err
	}
	var expanded bytes.Buffer
	encoder := json.NewEncoder(&expanded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "encode upload transaction segment", err)
	}
	canonical := bytes.TrimSuffix(expanded.Bytes(), []byte{'\n'})
	if len(canonical) == 0 || len(canonical) > MaxExpandedUploadTransactionSegmentBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "expanded upload transaction segment exceeds size limit")
	}
	compressed := encodeDeterministicGZIP(uploadTransactionSegmentMagic, canonical)
	if len(compressed) > MaxUploadTransactionSegmentBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "upload transaction segment exceeds size limit")
	}
	return compressed, nil
}

func DecodeUploadTransactionSegment(data []byte, backendKind, ownerID, transactionID, requestFingerprint, kind string, segment uint64) (UploadTransactionSegment, error) {
	if len(data) <= len(uploadTransactionSegmentMagic) || len(data) > MaxUploadTransactionSegmentBytes || !bytes.Equal(data[:len(uploadTransactionSegmentMagic)], uploadTransactionSegmentMagic) {
		return UploadTransactionSegment{}, domain.NewError(domain.ErrorInvalid, "invalid upload transaction segment envelope")
	}
	reader, err := gzip.NewReader(bytes.NewReader(data[len(uploadTransactionSegmentMagic):]))
	if err != nil {
		return UploadTransactionSegment{}, domain.WrapError(domain.ErrorInvalid, "open upload transaction segment", err)
	}
	expanded, readErr := io.ReadAll(io.LimitReader(reader, MaxExpandedUploadTransactionSegmentBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(expanded) == 0 || len(expanded) > MaxExpandedUploadTransactionSegmentBytes {
		return UploadTransactionSegment{}, domain.NewError(domain.ErrorInvalid, "expand upload transaction segment")
	}
	var value UploadTransactionSegment
	if err := state.DecodeJSONWithLimit(expanded, &value, MaxExpandedUploadTransactionSegmentBytes); err != nil {
		return UploadTransactionSegment{}, err
	}
	if value.BackendKind != backendKind || value.OwnerID != ownerID || value.TransactionID != transactionID || value.RequestFingerprint != requestFingerprint || value.Kind != kind || value.Segment != segment {
		return UploadTransactionSegment{}, domain.NewError(domain.ErrorInvalid, "upload transaction segment key binding mismatch")
	}
	if err := ValidateUploadTransactionSegment(value); err != nil {
		return UploadTransactionSegment{}, err
	}
	canonical, err := EncodeUploadTransactionSegment(value)
	if err != nil || !bytes.Equal(canonical, data) {
		return UploadTransactionSegment{}, domain.NewError(domain.ErrorInvalid, "non-canonical upload transaction segment encoding")
	}
	return value, nil
}
