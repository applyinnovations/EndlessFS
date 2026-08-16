package domain

import (
	"bytes"
	"encoding/base64"
	"errors"
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
