package domain

import (
	"bytes"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestIDGeneratorUsesRequiredEntropyAndUnpaddedBase64URL(t *testing.T) {
	t.Parallel()

	source := bytes.NewReader(bytes.Repeat([]byte{0x5a}, 128))
	generator := NewIDGenerator(source)
	userID, err := generator.UserID()
	if err != nil {
		t.Fatalf("UserID() error = %v", err)
	}
	token, err := generator.BearerToken()
	if err != nil {
		t.Fatalf("BearerToken() error = %v", err)
	}

	userBytes, err := base64.RawURLEncoding.DecodeString(userID.String())
	if err != nil || len(userBytes) != 16 {
		t.Fatalf("user ID decoded length = %d, error = %v", len(userBytes), err)
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		t.Fatalf("token decoded length = %d, error = %v", len(tokenBytes), err)
	}
}

func TestIDGeneratorRejectsShortRandomReads(t *testing.T) {
	t.Parallel()

	generator := NewIDGenerator(&failingReader{})
	if _, err := generator.UserID(); err == nil {
		t.Fatal("UserID() succeeded with broken randomness")
	}
}

func TestOwnerScopedOpaqueIDRoundTrip(t *testing.T) {
	owner, err := ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	raw := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, 16))
	scoped, err := ScopeOpaqueID(owner, raw)
	if err != nil {
		t.Fatal(err)
	}
	parsedOwner, parsedRaw, err := ParseScopedOpaqueID(scoped)
	if err != nil || parsedOwner != owner || parsedRaw != raw {
		t.Fatalf("parsed scoped ID = %v %q %v", parsedOwner, parsedRaw, err)
	}
	for _, invalid := range []string{"", raw, scoped + "x", base64.RawURLEncoding.EncodeToString([]byte{1, 0})} {
		if _, _, err := ParseScopedOpaqueID(invalid); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid scoped ID %q error = %v", invalid, err)
		}
	}
	oversizedOwner, err := ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x4f}, 1<<16)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScopeOpaqueID(oversizedOwner, raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized owner scoped ID error = %v", err)
	}
}

func TestIDGeneratorSerializesConcurrentEntropyReads(t *testing.T) {
	generator := NewIDGenerator(bytes.NewReader(deterministicIDBytes(64 * 16)))
	values := make(chan string, 64)
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, err := generator.OpaqueID()
			if err != nil {
				t.Errorf("OpaqueID() error = %v", err)
				return
			}
			values <- value
		}()
	}
	wait.Wait()
	close(values)
	seen := make(map[string]struct{}, 64)
	for value := range values {
		if _, duplicate := seen[value]; duplicate {
			t.Fatalf("duplicate concurrent ID %q", value)
		}
		seen[value] = struct{}{}
	}
	if len(seen) != 64 {
		t.Fatalf("generated IDs = %d, want 64", len(seen))
	}
}

func deterministicIDBytes(size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = byte(index/16) ^ byte(index*17)
	}
	return value
}

func TestScopeRequiresTrustedValidValues(t *testing.T) {
	t.Parallel()

	userID, err := ParseUserID("WlpaWlpaWlpaWlpaWlpaWg")
	if err != nil {
		t.Fatalf("ParseUserID() error = %v", err)
	}
	for _, area := range []Area{AreaLive, AreaTrash} {
		scope, err := NewScope(userID, area)
		if err != nil {
			t.Fatalf("NewScope() error = %v", err)
		}
		if scope.UserID() != userID || scope.Area() != area {
			t.Fatalf("scope = %+v", scope)
		}
	}
	if _, err := NewScope(UserID{}, AreaLive); err == nil {
		t.Fatal("NewScope() accepted empty user ID")
	}
	if _, err := NewScope(userID, Area(99)); err == nil {
		t.Fatal("NewScope() accepted invalid area")
	}
}

func TestFixedClockIsDeterministic(t *testing.T) {
	t.Parallel()

	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := NewFixedClock(want)
	if got := clock.Now(); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
	clock.Advance(time.Minute)
	if got := clock.Now(); !got.Equal(want.Add(time.Minute)) {
		t.Fatalf("advanced Now() = %v", got)
	}
}

type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}
