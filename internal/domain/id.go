package domain

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync"
	"time"
)

var scopedOpaqueIDPrefix = []byte("EFSOID1\x00")

type UserID struct {
	value string
}

func ParseUserID(value string) (UserID, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) < 16 || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return UserID{}, NewError(ErrorInvalid, "invalid user ID")
	}
	return UserID{value: value}, nil
}

func (id UserID) String() string {
	return id.value
}

func (id UserID) Valid() bool {
	return id.value != ""
}

func (id UserID) MarshalJSON() ([]byte, error) {
	if !id.Valid() {
		return nil, NewError(ErrorInvalid, "cannot encode invalid user ID")
	}
	return json.Marshal(id.value)
}

func (id *UserID) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := ParseUserID(value)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

type Area uint8

const (
	AreaLive Area = iota + 1
	AreaTrash
)

type Scope struct {
	userID UserID
	area   Area
}

func NewScope(userID UserID, area Area) (Scope, error) {
	if !userID.Valid() {
		return Scope{}, NewError(ErrorInvalid, "scope requires a user ID")
	}
	if area != AreaLive && area != AreaTrash {
		return Scope{}, NewError(ErrorInvalid, "scope requires a valid area")
	}
	return Scope{userID: userID, area: area}, nil
}

func (s Scope) UserID() UserID {
	return s.userID
}

func (s Scope) Area() Area {
	return s.area
}

func (s Scope) Valid() bool {
	return s.userID.Valid() && (s.area == AreaLive || s.area == AreaTrash)
}

type IDGenerator struct {
	mu     sync.Mutex
	source io.Reader
}

func NewIDGenerator(source io.Reader) *IDGenerator {
	return &IDGenerator{source: source}
}

func SystemIDGenerator() *IDGenerator {
	return NewIDGenerator(rand.Reader)
}

func (g *IDGenerator) UserID() (UserID, error) {
	value, err := g.bytes(16)
	if err != nil {
		return UserID{}, err
	}
	return UserID{value: value}, nil
}

func (g *IDGenerator) OpaqueID() (string, error) {
	return g.bytes(16)
}

func (g *IDGenerator) BearerToken() (string, error) {
	return g.bytes(32)
}

// ScopeOpaqueID creates a canonical opaque locator that can select an owner's
// consistency domain without exposing a provider key. The locator itself is
// not authorization and must still be bound to authenticated state.
func ScopeOpaqueID(owner UserID, opaque string) (string, error) {
	ownerBytes, ownerErr := base64.RawURLEncoding.DecodeString(owner.String())
	opaqueBytes, opaqueErr := base64.RawURLEncoding.DecodeString(opaque)
	if !owner.Valid() || ownerErr != nil || len(ownerBytes) < 16 || len(ownerBytes) > 65535 || base64.RawURLEncoding.EncodeToString(ownerBytes) != owner.String() || opaqueErr != nil || len(opaqueBytes) < 16 || base64.RawURLEncoding.EncodeToString(opaqueBytes) != opaque {
		return "", NewError(ErrorInvalid, "invalid owner-scoped opaque ID material")
	}
	encoded := make([]byte, 0, len(scopedOpaqueIDPrefix)+2+len(ownerBytes)+len(opaqueBytes))
	encoded = append(encoded, scopedOpaqueIDPrefix...)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(ownerBytes)))
	encoded = append(encoded, length...)
	encoded = append(encoded, ownerBytes...)
	encoded = append(encoded, opaqueBytes...)
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func ParseScopedOpaqueID(value string) (UserID, string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value || len(decoded) < len(scopedOpaqueIDPrefix)+2+16+16 || !bytes.HasPrefix(decoded, scopedOpaqueIDPrefix) {
		return UserID{}, "", NewError(ErrorInvalid, "invalid owner-scoped opaque ID")
	}
	offset := len(scopedOpaqueIDPrefix)
	ownerLength := int(binary.BigEndian.Uint16(decoded[offset : offset+2]))
	offset += 2
	if ownerLength < 16 || offset+ownerLength+16 > len(decoded) {
		return UserID{}, "", NewError(ErrorInvalid, "invalid owner-scoped opaque ID")
	}
	owner, err := ParseUserID(base64.RawURLEncoding.EncodeToString(decoded[offset : offset+ownerLength]))
	if err != nil {
		return UserID{}, "", NewError(ErrorInvalid, "invalid owner-scoped opaque ID")
	}
	opaque := base64.RawURLEncoding.EncodeToString(decoded[offset+ownerLength:])
	return owner, opaque, nil
}

func (g *IDGenerator) bytes(size int) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	value := make([]byte, size)
	if _, err := io.ReadFull(g.source, value); err != nil {
		return "", WrapError(ErrorInternal, "secure randomness unavailable", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}

type FixedClock struct {
	mu  sync.RWMutex
	now time.Time
}

func NewFixedClock(now time.Time) *FixedClock {
	return &FixedClock{now: now.UTC()}
}

func (c *FixedClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *FixedClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}
