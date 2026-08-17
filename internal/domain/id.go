package domain

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"
	"time"
)

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
