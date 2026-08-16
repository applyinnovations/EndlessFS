// Package state defines provider-neutral conditional application state.
package state

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type Namespace string

const (
	NamespaceBootstrap   Namespace = "bootstrap"
	NamespaceUsers       Namespace = "users"
	NamespaceAccounts    Namespace = "accounts"
	NamespaceCredentials Namespace = "credentials"
	NamespaceRoles       Namespace = "roles"
	NamespaceSessions    Namespace = "sessions"
	NamespaceCeremonies  Namespace = "ceremonies"
	NamespaceInvites     Namespace = "invites"
	NamespaceRecoveries  Namespace = "recoveries"
	NamespaceShares      Namespace = "shares"
	NamespaceTrash       Namespace = "trash"
	NamespaceUploads     Namespace = "uploads"
	NamespaceOperations  Namespace = "operations"
	NamespaceIdempotency Namespace = "idempotency"
	NamespacePreferences Namespace = "preferences"
)

var validNamespaces = map[Namespace]struct{}{
	NamespaceBootstrap: {}, NamespaceUsers: {}, NamespaceAccounts: {}, NamespaceCredentials: {},
	NamespaceRoles: {}, NamespaceSessions: {}, NamespaceCeremonies: {}, NamespaceInvites: {},
	NamespaceRecoveries: {}, NamespaceShares: {}, NamespaceTrash: {}, NamespaceUploads: {},
	NamespaceOperations: {}, NamespaceIdempotency: {}, NamespacePreferences: {},
}

// Key can only be assembled from a fixed namespace and encoded opaque parts.
type Key struct {
	value string
}

type Prefix struct {
	value string
}

func NewKey(namespace Namespace, parts ...string) (Key, error) {
	value, err := encodedPath(namespace, parts)
	if err != nil {
		return Key{}, err
	}
	return Key{value: value}, nil
}

func MustKey(namespace Namespace, parts ...string) Key {
	key, err := NewKey(namespace, parts...)
	if err != nil {
		panic(err)
	}
	return key
}

func NewPrefix(namespace Namespace, parts ...string) (Prefix, error) {
	value, err := encodedPath(namespace, parts)
	if err != nil {
		return Prefix{}, err
	}
	return Prefix{value: value + "/"}, nil
}

func MustPrefix(namespace Namespace, parts ...string) Prefix {
	prefix, err := NewPrefix(namespace, parts...)
	if err != nil {
		panic(err)
	}
	return prefix
}

func encodedPath(namespace Namespace, parts []string) (string, error) {
	if _, ok := validNamespaces[namespace]; !ok {
		return "", domain.NewError(domain.ErrorInvalid, "invalid state namespace")
	}
	encoded := make([]string, 1, len(parts)+1)
	encoded[0] = string(namespace)
	for _, part := range parts {
		if part == "" || !utf8.ValidString(part) {
			return "", domain.NewError(domain.ErrorInvalid, "invalid state key part")
		}
		encoded = append(encoded, base64.RawURLEncoding.EncodeToString([]byte(part)))
	}
	return strings.Join(encoded, "/"), nil
}

func (k Key) String() string {
	return k.value
}

func (k Key) Valid() bool {
	return k.value != ""
}

func (p Prefix) String() string {
	return p.value
}

func (p Prefix) Valid() bool {
	return p.value != ""
}

type Version string

type Value struct {
	Data    []byte
	Version Version
}

type Item struct {
	Key   Key
	Value Value
}

type PageRequest struct {
	Limit  int
	Cursor string
}

type Page struct {
	Items      []Item
	NextCursor string
}

type Store interface {
	Get(context.Context, Key) (Value, error)
	List(context.Context, Prefix, PageRequest) (Page, error)
	Create(context.Context, Key, []byte) (Version, error)
	CompareAndSwap(context.Context, Key, Version, []byte) (Version, error)
	Delete(context.Context, Key, Version) error
}

func normalizePageLimit(limit int) (int, error) {
	if limit == 0 {
		return 200, nil
	}
	if limit < 1 || limit > 1000 {
		return 0, domain.NewError(domain.ErrorInvalid, "page limit must be between 1 and 1000")
	}
	return limit, nil
}

func validateKey(key Key) error {
	if !key.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid state key")
	}
	return nil
}

func validatePrefix(prefix Prefix) error {
	if !prefix.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid state prefix")
	}
	return nil
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "state request canceled", err)
	}
	return nil
}

func versionString(counter uint64) Version {
	return Version(fmt.Sprintf("s%016x", counter))
}
