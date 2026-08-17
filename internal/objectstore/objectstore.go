// Package objectstore defines the narrow provider object transport boundary.
package objectstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
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

type ObjectInfo struct {
	Key     Key
	Version NativeVersion
	Size    int64
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
}

type ListPage struct {
	Objects    []ObjectInfo
	NextCursor string
}

type Backend interface {
	Head(context.Context, Key) (ObjectInfo, error)
	Get(context.Context, Key) (Object, error)
	List(context.Context, ListRequest) (ListPage, error)
	Put(context.Context, Key, []byte, PutCondition) (NativeVersion, error)
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
