// Package objectstore defines the narrow provider object transport boundary.
package objectstore

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
)

const (
	MaxKeyBytes    = 240
	MaxKeySegments = 24
)

type Key struct{ value string }

func ParseKey(value string) (Key, error) {
	if len(value) == 0 || len(value) > MaxKeyBytes || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return Key{}, domain.NewError(domain.ErrorInvalid, "invalid object key")
	}
	segments := strings.Split(value, "/")
	if len(segments) > MaxKeySegments {
		return Key{}, domain.NewError(domain.ErrorInvalid, "object key has too many segments")
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return Key{}, domain.NewError(domain.ErrorInvalid, "invalid object key segment")
		}
		for _, character := range segment {
			if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '.' {
				continue
			}
			return Key{}, domain.NewError(domain.ErrorInvalid, "invalid object key character")
		}
	}
	return Key{value: value}, nil
}

func MustKey(value string) Key {
	key, err := ParseKey(value)
	if err != nil {
		panic(err)
	}
	return key
}

func (k Key) String() string { return k.value }
func (k Key) Valid() bool {
	parsed, err := ParseKey(k.value)
	return err == nil && parsed == k
}

type NativeVersion string

type Object struct {
	Key     Key
	Body    []byte
	Version NativeVersion
	Size    int64
}

// ObjectReader streams one immutable object at a fixed native version. It is
// absent from the ordinary file-control boundary and is available only to an
// explicitly exempt optional feature such as image preview generation.
type ObjectReader struct {
	Key     Key
	Body    io.ReadCloser
	Version NativeVersion
	Size    int64
}

type ObjectInfo struct {
	Key         Key
	Version     NativeVersion
	Size        int64
	Fingerprint ContentFingerprint
}

// ContentFingerprint is normalized provider-attested content metadata. It
// contains algorithm results only; provider encodings, ETags, generations,
// multipart identifiers, and other native values never cross this boundary.
type ContentFingerprint struct {
	MD5    string
	CRC32C string
}

func (f ContentFingerprint) Validate() error {
	if _, ok := integrity.ParseMD5(f.MD5); !ok {
		return domain.NewError(domain.ErrorInvalid, "invalid MD5 content fingerprint")
	}
	if _, ok := integrity.ParseCRC32C(f.CRC32C); !ok {
		return domain.NewError(domain.ErrorInvalid, "invalid CRC32C content fingerprint")
	}
	return nil
}

func (f ContentFingerprint) Complete() bool { return f.Validate() == nil }

// FingerprintFor is for deterministic local backends and tests. Production
// services obtain stored-file fingerprints from provider metadata.
func FingerprintFor(body []byte) ContentFingerprint {
	return ContentFingerprint{MD5: integrity.MD5(body), CRC32C: integrity.CRC32C(body)}
}

// ExpectedIntegrity is a provider-independent assertion about one immutable
// object body. Production backends satisfy it from provider integrity
// metadata; the Go service never downloads a stored file merely to verify it.
// Native checksum encodings never cross this boundary or become durable state.
type ExpectedIntegrity struct {
	Size     int64
	Checksum Checksum
}

type ChecksumAlgorithm string

const ChecksumCRC32C ChecksumAlgorithm = "crc32c"

type Checksum struct {
	Algorithm ChecksumAlgorithm
	Value     string
}

func IntegrityFor(body []byte) ExpectedIntegrity {
	return ExpectedIntegrity{Size: int64(len(body)), Checksum: Checksum{Algorithm: ChecksumCRC32C, Value: integrity.CRC32C(body)}}
}

func (i ExpectedIntegrity) Validate() error {
	if i.Size >= 0 && i.Checksum.Algorithm == ChecksumCRC32C {
		if _, ok := integrity.ParseCRC32C(i.Checksum.Value); ok {
			return nil
		}
	}
	return domain.NewError(domain.ErrorInvalid, "invalid expected object integrity")
}

type PutMode uint8

const (
	PutCreateOnly PutMode = iota + 1
	PutMatch
)

type PutCondition struct {
	Mode    PutMode
	Version NativeVersion
}

func (c PutCondition) Validate() error {
	if c.Mode == PutCreateOnly && c.Version == "" {
		return nil
	}
	if c.Mode == PutMatch && c.Version != "" {
		return nil
	}
	return domain.NewError(domain.ErrorInvalid, "invalid object put condition")
}

type DeleteCondition struct{ Version NativeVersion }

type CopyCondition struct {
	SourceVersion NativeVersion
	Destination   PutCondition
}

type CopyResult struct {
	Version NativeVersion
	Size    int64
}

type ListRequest struct {
	Prefix string
	Limit  int
	Cursor string
	// After is a provider-neutral exclusive canonical-key continuation. It is
	// used by portable indexes whose client cursor must remain constant-size
	// and cannot durably depend on a provider page token.
	After string
}

func (r ListRequest) Validate() error {
	if err := ValidatePrefix(r.Prefix); err != nil {
		return err
	}
	if r.Cursor != "" && r.After != "" {
		return domain.NewError(domain.ErrorInvalid, "object list cursor and after key are mutually exclusive")
	}
	if r.After != "" {
		key, err := ParseKey(r.After)
		if err != nil || !strings.HasPrefix(key.String(), r.Prefix) {
			return domain.NewError(domain.ErrorInvalid, "invalid object list after key")
		}
	}
	return nil
}

type ListPage struct {
	Objects    []ObjectInfo
	NextCursor string
}

type Backend interface {
	Head(context.Context, Key) (ObjectInfo, error)
	Verify(context.Context, Key, ExpectedIntegrity) (ObjectInfo, error)
	Get(context.Context, Key) (Object, error)
	Open(context.Context, Key) (ObjectReader, error)
	List(context.Context, ListRequest) (ListPage, error)
	Put(context.Context, Key, []byte, PutCondition) (NativeVersion, error)
	Delete(context.Context, Key, DeleteCondition) error
	Copy(context.Context, Key, Key, CopyCondition) (CopyResult, error)
}

// MetadataBackend is the body-free object boundary used for file inventory
// and integrity decisions. It intentionally cannot read an object body.
type ListBackend interface {
	List(context.Context, ListRequest) (ListPage, error)
}

type MetadataBackend interface {
	ListBackend
	Head(context.Context, Key) (ObjectInfo, error)
	Verify(context.Context, Key, ExpectedIntegrity) (ObjectInfo, error)
}

// FileControlBackend adds only server-side file mutations to MetadataBackend.
// Direct browser transfers are negotiated through DirectTransferBackend.
// Neither interface exposes Get, Open, or Put to the Go file control plane.
type FileControlBackend interface {
	MetadataBackend
	Delete(context.Context, Key, DeleteCondition) error
	Copy(context.Context, Key, Key, CopyCondition) (CopyResult, error)
}

func ValidatePrefix(prefix string) error {
	if prefix == "" || len(prefix) > MaxKeyBytes+1 || !strings.HasSuffix(prefix, "/") {
		return domain.NewError(domain.ErrorInvalid, "invalid object prefix")
	}
	_, err := ParseKey(strings.TrimSuffix(prefix, "/"))
	return err
}

func ContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "object request canceled", err)
	}
	return nil
}

func VersionString(prefix string, counter uint64) NativeVersion {
	return NativeVersion(fmt.Sprintf("%s%016x", prefix, counter))
}
