package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestStateKeysAndMemoryStoreRejectEveryBoundary(t *testing.T) {
	key, err := NewKey(NamespaceAccounts, "owner", "record")
	if err != nil || !key.Valid() || key.String() == "" {
		t.Fatalf("NewKey() = %#v, %v", key, err)
	}
	prefix, err := NewPrefix(NamespaceAccounts, "owner")
	if err != nil || !prefix.Valid() || !strings.HasSuffix(prefix.String(), "/") {
		t.Fatalf("NewPrefix() = %#v, %v", prefix, err)
	}
	for name, build := range map[string]func() error{
		"unknown namespace": func() error { _, err := NewKey("unknown", "part"); return err },
		"empty part":        func() error { _, err := NewKey(NamespaceUsers, ""); return err },
		"invalid UTF-8":     func() error { _, err := NewPrefix(NamespaceUsers, string([]byte{0xff})); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := build(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("boundary error = %v", err)
			}
		})
	}

	store := NewMemoryStore()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() error{
		"get context":    func() error { _, err := store.Get(canceled, key); return err },
		"list context":   func() error { _, err := store.List(canceled, prefix, PageRequest{}); return err },
		"create context": func() error { _, err := store.Create(canceled, key, nil); return err },
		"swap context":   func() error { _, err := store.CompareAndSwap(canceled, key, "v", nil); return err },
		"delete context": func() error { return store.Delete(canceled, key, "v") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("canceled operation = %v", err)
			}
		})
	}
	if _, err := store.Get(context.Background(), Key{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Get key = %v", err)
	}
	if _, err := store.List(context.Background(), Prefix{}, PageRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid List prefix = %v", err)
	}
	for _, limit := range []int{-1, 1001} {
		if _, err := store.List(context.Background(), prefix, PageRequest{Limit: limit}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("List limit %d = %v", limit, err)
		}
	}
	if _, err := store.List(context.Background(), prefix, PageRequest{Cursor: "unknown"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown cursor = %v", err)
	}
	if _, err := store.Create(context.Background(), Key{}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Create key = %v", err)
	}
	version, err := store.Create(context.Background(), key, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), key, []byte("duplicate")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate Create = %v", err)
	}
	if _, err := store.CompareAndSwap(context.Background(), Key{}, version, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid swap key = %v", err)
	}
	if _, err := store.CompareAndSwap(context.Background(), key, "", nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty swap version = %v", err)
	}
	missing := MustKey(NamespaceAccounts, "missing")
	if _, err := store.CompareAndSwap(context.Background(), missing, version, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing swap = %v", err)
	}
	if _, err := store.CompareAndSwap(context.Background(), key, "stale", nil); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale swap = %v", err)
	}
	next, err := store.CompareAndSwap(context.Background(), key, version, []byte("second"))
	if err != nil || next == version {
		t.Fatalf("valid swap = %q, %v", next, err)
	}
	if err := store.Delete(context.Background(), Key{}, next); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid delete key = %v", err)
	}
	if err := store.Delete(context.Background(), key, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty delete version = %v", err)
	}
	if err := store.Delete(context.Background(), missing, next); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing delete = %v", err)
	}
	if err := store.Delete(context.Background(), key, "stale"); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale delete = %v", err)
	}
	if err := store.Delete(context.Background(), key, next); err != nil {
		t.Fatalf("valid delete = %v", err)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted Get = %v", err)
	}
}

func TestStateCodecCoversValidationMarshalAndNestedJSON(t *testing.T) {
	if _, err := EncodeJSON(&testRecord{SchemaVersion: 2}); err == nil {
		t.Fatal("EncodeJSON accepted an invalid record")
	}
	if _, err := EncodeJSON(make(chan int)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("EncodeJSON unsupported value = %v", err)
	}
	oversized := strings.Repeat("x", MaxRecordBytes+1)
	if _, err := EncodeJSON(oversized); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("EncodeJSON oversized = %v", err)
	}
	for _, data := range []string{
		`{"schemaVersion":1,"name":"safe","nested":{"one":"1","two":"2"}}`,
		`{"schemaVersion":1,"name":"safe","nested":{"one":"1","one":"2"}}`,
		`[{"nested":[1,2,3]}]`,
		`{"schemaVersion":1,"name":"safe"} trailing`,
	} {
		var record testRecord
		_ = DecodeJSONWithLimit([]byte(data), &record, MaxRecordBytes)
	}
	var record testRecord
	if err := DecodeJSONWithLimit([]byte(`{}`), &record, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero decode limit = %v", err)
	}
}

func TestMustStateConstructorsPanicForInvalidValues(t *testing.T) {
	for name, call := range map[string]func(){
		"key":    func() { _ = MustKey("invalid", "part") },
		"prefix": func() { _ = MustPrefix("invalid", "part") },
	} {
		t.Run(name, func(t *testing.T) {
			deferred := false
			func() {
				defer func() { deferred = recover() != nil }()
				call()
			}()
			if !deferred {
				t.Fatal("constructor did not panic")
			}
		})
	}
}
