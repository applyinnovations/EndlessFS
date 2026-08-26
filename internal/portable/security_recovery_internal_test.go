package portable

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type hookedBackend struct {
	objectstore.Backend
	head   func(context.Context, objectstore.Key) (objectstore.ObjectInfo, error)
	get    func(context.Context, objectstore.Key) (objectstore.Object, error)
	open   func(context.Context, objectstore.Key) (objectstore.ObjectReader, error)
	list   func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error)
	put    func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error)
	delete func(context.Context, objectstore.Key, objectstore.DeleteCondition) error
	copy   func(context.Context, objectstore.Key, objectstore.Key, objectstore.CopyCondition) (objectstore.CopyResult, error)
}

func (backend *hookedBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if backend.head != nil {
		return backend.head(ctx, key)
	}
	return backend.Backend.Head(ctx, key)
}

func (backend *hookedBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if backend.get != nil {
		return backend.get(ctx, key)
	}
	return backend.Backend.Get(ctx, key)
}

func (backend *hookedBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	if backend.open != nil {
		return backend.open(ctx, key)
	}
	return backend.Backend.Open(ctx, key)
}

func (backend *hookedBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if backend.list != nil {
		return backend.list(ctx, request)
	}
	return backend.Backend.List(ctx, request)
}

func (backend *hookedBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if backend.put != nil {
		return backend.put(ctx, key, body, condition)
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func (backend *hookedBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if backend.delete != nil {
		return backend.delete(ctx, key, condition)
	}
	return backend.Backend.Delete(ctx, key, condition)
}

func (backend *hookedBackend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	if backend.copy != nil {
		return backend.copy(ctx, source, destination, condition)
	}
	return backend.Backend.Copy(ctx, source, destination, condition)
}

func TestMutationRecoveryRejectsCorruptionAndConvergesAfterRaces(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2044, 2, 3, 4, 5, 6, 0, time.UTC))
	newEngine := func(t *testing.T) (*objectmemory.Backend, *hookedBackend, *Engine) {
		t.Helper()
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openInternalTestEngine(t, hooks, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		return memory, hooks, engine
	}
	admission := func(t *testing.T, intent *storageformat.MutationIntent) storageformat.Admission {
		t.Helper()
		result := storageformat.Admission{Mutation: intent}
		if intent != nil {
			encoded, err := storageformat.EncodeCanonical(*intent)
			if err != nil {
				t.Fatal(err)
			}
			result.IntentDigest = storageformat.Digest(encoded)
		}
		return result
	}

	t.Run("missing-intent", func(t *testing.T) {
		_, _, engine := newEngine(t)
		if err := engine.recoverMutation(ctx, storageformat.Admission{}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("recoverMutation() error = %v", err)
		}
	})
	t.Run("digest-mismatch", func(t *testing.T) {
		_, _, engine := newEngine(t)
		intent := &storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: "valid", TargetBody: []byte("body")}
		candidate := admission(t, intent)
		candidate.IntentDigest = "wrong"
		if err := engine.recoverMutation(ctx, candidate); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("recoverMutation() error = %v", err)
		}
	})
	t.Run("invalid-recovery-keys", func(t *testing.T) {
		for _, mutate := range []func(*storageformat.MutationIntent){
			func(intent *storageformat.MutationIntent) { intent.RecoverUploadKey = "INVALID" },
			func(intent *storageformat.MutationIntent) { intent.TargetKey = "INVALID" },
		} {
			_, _, engine := newEngine(t)
			intent := &storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: "valid", TargetBody: []byte("body")}
			mutate(intent)
			if err := engine.recoverMutation(ctx, admission(t, intent)); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("recoverMutation() error = %v", err)
			}
		}
	})
	t.Run("create-conflicting-winner-is-terminal", func(t *testing.T) {
		_, hooks, engine := newEngine(t)
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorConflict, "winner")
		}
		intent := &storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: "valid", TargetBody: []byte("body")}
		if err := engine.recoverMutation(ctx, admission(t, intent)); err != nil {
			t.Fatalf("recoverMutation() error = %v", err)
		}
	})

	for _, action := range []storageformat.MutationAction{storageformat.MutationCAS, storageformat.MutationDelete} {
		action := action
		t.Run(string(action)+"-read-failure", func(t *testing.T) {
			_, hooks, engine := newEngine(t)
			hooks.get = func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key.String() == "valid" {
					return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "read failed")
				}
				return hooks.Backend.Get(ctx, key)
			}
			intent := &storageformat.MutationIntent{Action: action, TargetKey: "valid", ExpectedLogicalVersion: "expected", TargetBody: []byte("body")}
			if err := engine.recoverMutation(ctx, admission(t, intent)); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("recoverMutation() error = %v", err)
			}
		})
		t.Run(string(action)+"-malformed-current", func(t *testing.T) {
			memory, _, engine := newEngine(t)
			if _, err := memory.Put(ctx, objectstore.MustKey("valid"), []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			intent := &storageformat.MutationIntent{Action: action, TargetKey: "valid", ExpectedLogicalVersion: "expected", TargetBody: []byte("body")}
			if err := engine.recoverMutation(ctx, admission(t, intent)); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("recoverMutation() error = %v", err)
			}
		})
		t.Run(string(action)+"-missing-logical-version", func(t *testing.T) {
			memory, _, engine := newEngine(t)
			if _, err := memory.Put(ctx, objectstore.MustKey("valid"), []byte(`{"schema":"x"}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			intent := &storageformat.MutationIntent{Action: action, TargetKey: "valid", ExpectedLogicalVersion: "expected", TargetBody: []byte("body")}
			if err := engine.recoverMutation(ctx, admission(t, intent)); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("recoverMutation() error = %v", err)
			}
		})
		t.Run(string(action)+"-changed-version-is-terminal", func(t *testing.T) {
			memory, _, engine := newEngine(t)
			body := encodeInternalEnvelope(t, "test-v1", objectstore.MustKey("valid"), 1, map[string]string{"value": "current"})
			if _, err := memory.Put(ctx, objectstore.MustKey("valid"), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			intent := &storageformat.MutationIntent{Action: action, TargetKey: "valid", ExpectedLogicalVersion: "different", TargetBody: []byte("body")}
			if err := engine.recoverMutation(ctx, admission(t, intent)); err != nil {
				t.Fatalf("recoverMutation() error = %v", err)
			}
		})
	}

	t.Run("cas-and-delete-lost-write-races-are-terminal", func(t *testing.T) {
		for _, action := range []storageformat.MutationAction{storageformat.MutationCAS, storageformat.MutationDelete} {
			memory, hooks, engine := newEngine(t)
			key := objectstore.MustKey("valid")
			body := encodeInternalEnvelope(t, "test-v1", key, 1, map[string]string{"value": "current"})
			var envelope storageformat.Envelope
			if err := storageformat.DecodeEnvelope(body, key, "test-v1", &envelope, &map[string]string{}); err != nil {
				t.Fatal(err)
			}
			if _, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			if action == storageformat.MutationCAS {
				hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
					return "", domain.NewError(domain.ErrorPreconditionFailed, "lost race")
				}
			} else {
				hooks.delete = func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
					return domain.NewError(domain.ErrorPreconditionFailed, "lost race")
				}
			}
			intent := &storageformat.MutationIntent{Action: action, TargetKey: key.String(), ExpectedLogicalVersion: envelope.LogicalVersion, TargetBody: []byte("replacement")}
			if err := engine.recoverMutation(ctx, admission(t, intent)); err != nil {
				t.Fatalf("recoverMutation(%s) error = %v", action, err)
			}
		}
	})
	t.Run("unknown-action", func(t *testing.T) {
		_, _, engine := newEngine(t)
		intent := &storageformat.MutationIntent{Action: "unknown", TargetKey: "valid"}
		if err := engine.recoverMutation(ctx, admission(t, intent)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("recoverMutation() error = %v", err)
		}
	})
}

func TestMutationRecoveryDependencyValidationMatrix(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2044, 3, 4, 5, 6, 7, 0, time.UTC))
	newEngine := func(t *testing.T) (*objectmemory.Backend, *hookedBackend, *Engine) {
		t.Helper()
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		return memory, hooks, openInternalTestEngine(t, hooks, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
	}

	t.Run("prerequisites", func(t *testing.T) {
		for _, objects := range [][]storageformat.MutationObject{
			{{Key: "b", Body: []byte("x")}, {Key: "a", Body: []byte("x")}},
			{{Key: "a"}},
			{{Key: "INVALID", Body: []byte("x")}},
		} {
			_, _, engine := newEngine(t)
			if err := engine.ensureMutationPrerequisitesForRecovery(ctx, objects, ""); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("prerequisites error = %v", err)
			}
		}
		memory, _, engine := newEngine(t)
		if _, err := memory.Put(ctx, objectstore.MustKey("a"), []byte("current"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.ensureMutationPrerequisitesForRecovery(ctx, []storageformat.MutationObject{{Key: "a", Body: []byte("wanted")}}, ""); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("collision error = %v", err)
		}
		if err := engine.ensureMutationPrerequisitesForRecovery(ctx, []storageformat.MutationObject{{Key: "a", Body: []byte("wanted")}}, "a"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed operation collision error = %v", err)
		}

		_, hooks, engine := newEngine(t)
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorConflict, "winner")
		}
		reads := 0
		hooks.get = func(context.Context, objectstore.Key) (objectstore.Object, error) {
			reads++
			if reads == 1 {
				return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "missing")
			}
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "winner unreadable")
		}
		if err := engine.ensureMutationPrerequisitesForRecovery(ctx, []storageformat.MutationObject{{Key: "a", Body: []byte("wanted")}}, ""); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("winner error = %v", err)
		}
	})

	t.Run("copies", func(t *testing.T) {
		for _, copies := range [][]storageformat.MutationCopy{
			{{SourceKey: "a", DestinationKey: "a", Size: 1}},
			{{SourceKey: "INVALID", DestinationKey: "b", Size: 1}},
			{{SourceKey: "a", DestinationKey: "INVALID", Size: 1}},
		} {
			_, _, engine := newEngine(t)
			if err := engine.ensureMutationCopies(ctx, copies); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("copies error = %v", err)
			}
		}
		memory, _, engine := newEngine(t)
		if _, err := memory.Put(ctx, objectstore.MustKey("destination"), []byte("xx"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.ensureMutationCopies(ctx, []storageformat.MutationCopy{{SourceKey: "source", DestinationKey: "destination", Size: 1}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("destination collision error = %v", err)
		}
		memory, _, engine = newEngine(t)
		if _, err := memory.Put(ctx, objectstore.MustKey("source"), []byte("xx"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.ensureMutationCopies(ctx, []storageformat.MutationCopy{{SourceKey: "source", DestinationKey: "destination", Size: 1}}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("source size error = %v", err)
		}
		memory, hooks, engine := newEngine(t)
		if _, err := memory.Put(ctx, objectstore.MustKey("source"), []byte("x"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks.copy = func(context.Context, objectstore.Key, objectstore.Key, objectstore.CopyCondition) (objectstore.CopyResult, error) {
			return objectstore.CopyResult{}, domain.NewError(domain.ErrorConflict, "winner")
		}
		if err := engine.ensureMutationCopies(ctx, []storageformat.MutationCopy{{SourceKey: "source", DestinationKey: "destination", Size: 1}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unreadable winner error = %v", err)
		}
	})

	t.Run("upload-aborts", func(t *testing.T) {
		_, _, engine := newEngine(t)
		if err := engine.ensureUploadAborts(ctx, []string{"upload"}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("metadata-only error = %v", err)
		}
		if err := engine.ensureUploadAborts(ctx, nil); err != nil {
			t.Fatalf("empty aborts error = %v", err)
		}
	})
}

func TestGateDrainRejectsInvalidDurableRecords(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2044, 6, 7, 8, 9, 10, 0, time.UTC))
	newEngine := func(t *testing.T) (*objectmemory.Backend, *hookedBackend, *Engine) {
		t.Helper()
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		return memory, hooks, openInternalTestEngine(t, hooks, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<16)))
	}
	put := func(t *testing.T, memory *objectmemory.Backend, key objectstore.Key, body []byte) objectstore.Object {
		t.Helper()
		if _, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		object, err := memory.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}

	for name, body := range map[string][]byte{
		"malformed":      []byte("not-json"),
		"wrong-schema":   encodeInternalEnvelope(t, "other-v1", objectstore.MustKey("endlessfs/v1/operations/test"), 1, map[string]string{"value": "x"}),
		"invalid-upload": encodeInternalEnvelope(t, uploadRecordSchema, objectstore.MustKey("endlessfs/v1/operations/other"), 1, storageformat.UploadRecord{}),
	} {
		t.Run("upload-"+name, func(t *testing.T) {
			memory, _, candidate := newEngine(t)
			put(t, memory, objectstore.MustKey("endlessfs/v1/operations/test"), body)
			err := candidate.drainOperationRecords(ctx, false, true)
			if name == "wrong-schema" {
				if err != nil {
					t.Fatalf("drainActiveUploads() error = %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid upload record was accepted")
			}
		})
	}

	admissionKey := storageformat.AdmissionKey(1, "operation")
	validAdmission := storageformat.Admission{
		SchemaVersion: 1, Epoch: 1, OperationID: "operation", WriterSetID: "writer", ReplicaAttemptID: "replica",
		ObservedGate: "gate", State: storageformat.AdmissionCandidate, Attempt: 1, Fence: 1,
		CreatedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Minute), IntentDigest: "digest",
	}
	for name, mutate := range map[string]func(*storageformat.Admission){
		"wrong-epoch":   func(value *storageformat.Admission) { value.Epoch = 2 },
		"wrong-writer":  func(value *storageformat.Admission) { value.WriterSetID = "other" },
		"invalid-state": func(value *storageformat.Admission) { value.State = "unknown" },
	} {
		t.Run("admission-"+name, func(t *testing.T) {
			memory, _, candidate := newEngine(t)
			value := validAdmission
			mutate(&value)
			put(t, memory, admissionKey, encodeInternalEnvelope(t, admissionSchema, admissionKey, 1, value))
			if err := candidate.drainAdmissions(ctx, 1); err == nil {
				t.Fatal("invalid admission record was accepted")
			}
		})
	}
	t.Run("admission-malformed", func(t *testing.T) {
		memory, _, candidate := newEngine(t)
		put(t, memory, admissionKey, []byte("not-json"))
		if err := candidate.drainAdmissions(ctx, 1); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("drainAdmissions() error = %v", err)
		}
	})

	t.Run("cancelled-admission-delete-failure", func(t *testing.T) {
		memory, hooks, candidate := newEngine(t)
		value := validAdmission
		value.State = storageformat.AdmissionCancelled
		object := put(t, memory, admissionKey, encodeInternalEnvelope(t, admissionSchema, admissionKey, 1, value))
		hooks.delete = func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
			return domain.NewError(domain.ErrorUnavailable, "delete failed")
		}
		if err := candidate.cancelAndRemoveAdmission(ctx, object, storageformat.Envelope{}, value); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("cancelAndRemoveAdmission() error = %v", err)
		}
	})

	for name, body := range map[string][]byte{
		"malformed":    []byte("not-json"),
		"other-schema": encodeInternalEnvelope(t, "other-v1", objectstore.MustKey("endlessfs/v1/operations/test"), 1, map[string]string{"value": "x"}),
	} {
		t.Run("operation-"+name, func(t *testing.T) {
			memory, _, candidate := newEngine(t)
			put(t, memory, objectstore.MustKey("endlessfs/v1/operations/test"), body)
			err := candidate.drainOperationRecords(ctx, true, false)
			if name == "other-schema" {
				if err != nil {
					t.Fatalf("drainFileOperations() error = %v", err)
				}
			} else if !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("drainFileOperations() error = %v", err)
			}
		})
	}

	t.Run("prune-invalid-state-record", func(t *testing.T) {
		memory, _, candidate := newEngine(t)
		key := objectstore.MustKey(storageformat.StateRecordsPrefix() + "invalid")
		put(t, memory, key, encodeInternalEnvelope(t, stateRecordSchema, key, 1, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: "INVALID", Data: []byte("x")}))
		if err := candidate.pruneStateVersions(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("pruneStateVersions() error = %v", err)
		}
	})
}

func TestGateDrainHandlesDisappearingAndExpiredWork(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2044, 7, 8, 9, 10, 11, 0, time.UTC))
	newEngine := func(t *testing.T, backend objectstore.Backend) *Engine {
		t.Helper()
		return openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<16)))
	}

	t.Run("listed-record-disappears", func(t *testing.T) {
		for name, key := range map[string]objectstore.Key{
			"upload":    objectstore.MustKey("endlessfs/v1/operations/disappeared"),
			"admission": storageformat.AdmissionKey(1, "disappeared"),
			"operation": objectstore.MustKey("endlessfs/v1/operations/disappeared"),
		} {
			t.Run(name, func(t *testing.T) {
				memory := objectmemory.New()
				hooks := &hookedBackend{Backend: memory}
				hooks.list = func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
					return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: key}}}, nil
				}
				candidate := newEngine(t, hooks)
				var err error
				switch name {
				case "upload":
					err = candidate.drainOperationRecords(ctx, false, true)
				case "admission":
					err = candidate.drainAdmissions(ctx, 1)
				case "operation":
					err = candidate.drainOperationRecords(ctx, true, false)
				}
				if err != nil {
					t.Fatalf("drain error = %v", err)
				}
			})
		}
	})

	t.Run("active-admission-blocks-close", func(t *testing.T) {
		memory := objectmemory.New()
		candidate := newEngine(t, memory)
		key := storageformat.AdmissionKey(1, "active")
		value := storageformat.Admission{
			SchemaVersion: 1, Epoch: 1, OperationID: "active", WriterSetID: "writer", ReplicaAttemptID: "replica",
			ObservedGate: "gate", State: storageformat.AdmissionAdmitted, Attempt: 1, Fence: 1,
			CreatedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Minute), IntentDigest: "digest",
		}
		body := encodeInternalEnvelope(t, admissionSchema, key, 1, value)
		if _, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := candidate.drainAdmissions(ctx, 1); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("drainAdmissions() error = %v", err)
		}
	})

	t.Run("candidate-cancel-write-failure", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		candidate := newEngine(t, hooks)
		key := storageformat.AdmissionKey(1, "candidate")
		value := storageformat.Admission{State: storageformat.AdmissionCandidate}
		body := encodeInternalEnvelope(t, admissionSchema, key, 1, value)
		version, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if err != nil {
			t.Fatal(err)
		}
		object, err := memory.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		object.Version = version
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorUnavailable, "write failed")
		}
		if err := candidate.cancelAndRemoveAdmission(ctx, object, storageformat.Envelope{Revision: 1}, value); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("cancelAndRemoveAdmission() error = %v", err)
		}
	})

	t.Run("takeover-races", func(t *testing.T) {
		for name, failID := range map[string]bool{"id-failure": true, "conditional-write": false} {
			t.Run(name, func(t *testing.T) {
				memory := objectmemory.New()
				hooks := &hookedBackend{Backend: memory}
				candidate := newEngine(t, hooks)
				intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: "existing", TargetBody: []byte("body")}
				encoded, err := storageformat.EncodeCanonical(intent)
				if err != nil {
					t.Fatal(err)
				}
				admission := storageformat.Admission{Mutation: &intent, IntentDigest: storageformat.Digest(encoded)}
				if _, err := memory.Put(ctx, objectstore.MustKey("existing"), []byte("winner"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
					t.Fatal(err)
				}
				if failID {
					candidate.ids = domain.NewIDGenerator(strings.NewReader(""))
				} else {
					hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
						return "", domain.NewError(domain.ErrorPreconditionFailed, "lost takeover")
					}
				}
				if err := candidate.takeoverAndRecover(ctx, objectstore.Object{Key: storageformat.AdmissionKey(1, "takeover"), Version: "version"}, storageformat.Envelope{Revision: 1}, admission); err == nil {
					t.Fatal("takeover unexpectedly succeeded")
				}
			})
		}
	})

	t.Run("recovery-dependency-errors", func(t *testing.T) {
		for name, mutate := range map[string]func(*storageformat.MutationIntent){
			"upload": func(value *storageformat.MutationIntent) { value.RecoverUploadKey = "endlessfs/v1/operations/missing" },
			"copy": func(value *storageformat.MutationIntent) {
				value.Copies = []storageformat.MutationCopy{{SourceKey: "a", DestinationKey: "a", Size: 1}}
			},
			"abort": func(value *storageformat.MutationIntent) { value.AbortUploads = []string{"same", "same"} },
		} {
			t.Run(name, func(t *testing.T) {
				candidate := newEngine(t, objectmemory.New())
				intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: "valid", TargetBody: []byte("body")}
				mutate(&intent)
				encoded, err := storageformat.EncodeCanonical(intent)
				if err != nil {
					t.Fatal(err)
				}
				if err := candidate.recoverMutation(ctx, storageformat.Admission{Mutation: &intent, IntentDigest: storageformat.Digest(encoded)}); err == nil {
					t.Fatal("invalid recovery dependency was accepted")
				}
			})
		}
	})

	t.Run("missing-cas-target-is-terminal", func(t *testing.T) {
		candidate := newEngine(t, objectmemory.New())
		intent := storageformat.MutationIntent{Action: storageformat.MutationCAS, TargetKey: "missing", ExpectedLogicalVersion: "expected", TargetBody: []byte("body")}
		encoded, err := storageformat.EncodeCanonical(intent)
		if err != nil {
			t.Fatal(err)
		}
		if err := candidate.recoverMutation(ctx, storageformat.Admission{Mutation: &intent, IntentDigest: storageformat.Digest(encoded)}); err != nil {
			t.Fatalf("recoverMutation() error = %v", err)
		}
	})

	t.Run("invalid-upload-abort-order", func(t *testing.T) {
		memory := objectmemory.New()
		server := httptest.NewServer(memory)
		t.Cleanup(server.Close)
		if err := memory.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(strings.NewReader(strings.Repeat("upload", 1<<14)))); err != nil {
			t.Fatal(err)
		}
		candidate := newEngine(t, memory)
		if err := candidate.ensureUploadAborts(ctx, []string{"same", "same"}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("ensureUploadAborts() error = %v", err)
		}
	})
}

func TestGateCleanupFailuresAreFailClosed(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2044, 7, 9, 9, 10, 11, 0, time.UTC))
	newEngine := func(t *testing.T) (*objectmemory.Backend, *hookedBackend, *Engine) {
		t.Helper()
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		return memory, hooks, openInternalTestEngine(t, hooks, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<16)))
	}

	t.Run("malformed-state-record", func(t *testing.T) {
		memory, _, engine := newEngine(t)
		key := objectstore.MustKey(storageformat.StateRecordsPrefix() + "malformed")
		if _, err := memory.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("pruneStateVersions() error = %v", err)
		}
	})

	t.Run("orphan-version-delete-failure", func(t *testing.T) {
		memory, hooks, engine := newEngine(t)
		key := objectstore.MustKey(storageformat.StateVersionsPrefix() + "orphan")
		if _, err := memory.Put(ctx, key, []byte("orphan"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks.delete = func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
			return domain.NewError(domain.ErrorUnavailable, "delete failed")
		}
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("pruneStateVersions() error = %v", err)
		}
	})
}

func TestPortableInitializationAndCheckpointCorruptionMatrix(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2044, 9, 10, 11, 12, 13, 0, time.UTC))
	writer := WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}}
	options := func(backend objectstore.Backend) Options {
		return Options{Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(strings.NewReader(strings.Repeat("seed", 1<<16))), Writer: writer, LeaseTTL: time.Minute, CursorKey: []byte("0123456789abcdef0123456789abcdef")}
	}

	t.Run("open-invalid-writer", func(t *testing.T) {
		candidate := options(objectmemory.New())
		candidate.Writer = WriterConfiguration{}
		if _, err := Open(ctx, candidate); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("Open() error = %v", err)
		}
	})
	t.Run("initialize-randomness-failure", func(t *testing.T) {
		storedWriter, err := canonicalWriterConfiguration(writer)
		if err != nil {
			t.Fatal(err)
		}
		engine := &Engine{backend: objectmemory.New(), clock: clock, ids: domain.NewIDGenerator(strings.NewReader("")), writer: storedWriter}
		if err := engine.initialize(ctx); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("initialize() error = %v", err)
		}
	})
	t.Run("initialize-backend-failure", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorUnavailable, "write failed")
		}}
		if _, err := Open(ctx, options(hooks)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Open() error = %v", err)
		}
	})
	t.Run("initialize-conflict-winner-unreadable", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorConflict, "winner")
		}
		hooks.get = func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "winner unreadable")
		}
		if _, err := Open(ctx, options(hooks)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Open() error = %v", err)
		}
	})
	t.Run("initialize-malformed-superblock", func(t *testing.T) {
		backend := objectmemory.New()
		if _, err := backend.Put(ctx, storageformat.SuperblockKey(), []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, options(backend)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("Open() error = %v", err)
		}
	})

	t.Run("create-or-verify-failures", func(t *testing.T) {
		storedWriter, err := canonicalWriterConfiguration(writer)
		if err != nil {
			t.Fatal(err)
		}
		for name, payload := range map[string]any{
			"writer-malformed": storedWriter,
			"gate-malformed":   storageformat.WriteGate{SchemaVersion: 1, Epoch: 1, Mode: storageformat.GateOpen},
		} {
			t.Run(name, func(t *testing.T) {
				hooks := &hookedBackend{Backend: objectmemory.New()}
				hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
					return "", domain.NewError(domain.ErrorConflict, "winner")
				}
				hooks.get = func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
					return objectstore.Object{Key: key, Body: []byte("not-json")}, nil
				}
				engine := &Engine{backend: hooks}
				if err := engine.createOrVerifyEnvelope(ctx, objectstore.MustKey("test/key"), "test-v1", payload); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("createOrVerifyEnvelope() error = %v", err)
				}
			})
		}
		t.Run("put-failure", func(t *testing.T) {
			hooks := &hookedBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorUnavailable, "write failed")
			}}
			if err := (&Engine{backend: hooks}).createOrVerifyEnvelope(ctx, objectstore.MustKey("test/key"), "test-v1", storedWriter); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("createOrVerifyEnvelope() error = %v", err)
			}
		})
		t.Run("winner-read-failure", func(t *testing.T) {
			hooks := &hookedBackend{Backend: objectmemory.New()}
			hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "winner")
			}
			hooks.get = func(context.Context, objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "winner unreadable")
			}
			if err := (&Engine{backend: hooks}).createOrVerifyEnvelope(ctx, objectstore.MustKey("test/key"), "test-v1", storedWriter); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("createOrVerifyEnvelope() error = %v", err)
			}
		})
		t.Run("invalid-gate", func(t *testing.T) {
			hooks := &hookedBackend{Backend: objectmemory.New()}
			hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "winner")
			}
			key := objectstore.MustKey("test/key")
			hooks.get = func(context.Context, objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{Key: key, Body: encodeInternalEnvelope(t, "test-v1", key, 1, storageformat.WriteGate{})}, nil
			}
			if err := (&Engine{backend: hooks}).createOrVerifyEnvelope(ctx, key, "test-v1", storageformat.WriteGate{SchemaVersion: 1, Epoch: 1, Mode: storageformat.GateOpen}); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("createOrVerifyEnvelope() error = %v", err)
			}
		})
	})

	fixtureBackend := objectmemory.New()
	fixtureEngine, err := Open(ctx, options(fixtureBackend))
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureBackend.Export()
	for name, mutate := range map[string]func(map[string][]byte){
		"missing-superblock":   func(values map[string][]byte) { delete(values, storageformat.SuperblockKey().String()) },
		"malformed-superblock": func(values map[string][]byte) { values[storageformat.SuperblockKey().String()] = []byte("not-json") },
		"missing-writer":       func(values map[string][]byte) { delete(values, storageformat.WriterSetKey().String()) },
		"malformed-writer":     func(values map[string][]byte) { values[storageformat.WriterSetKey().String()] = []byte("not-json") },
	} {
		t.Run("readonly-"+name, func(t *testing.T) {
			values := cloneInternalObjects(fixture)
			mutate(values)
			backend := objectmemory.New()
			if err := backend.Import(values); err != nil {
				t.Fatal(err)
			}
			if err := VerifyCheckpointReadOnly(ctx, backend, writer, "checkpoint"); err == nil {
				t.Fatal("corrupt bucket was accepted")
			}
		})
	}
	if err := VerifyCheckpointReadOnly(ctx, fixtureBackend, WriterConfiguration{}, "checkpoint"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("VerifyCheckpointReadOnly(invalid writer) error = %v", err)
	}

	t.Run("checkpoint-superblock-corruption", func(t *testing.T) {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		engine, err := Open(ctx, options(backend))
		if err != nil {
			t.Fatal(err)
		}
		object, err := backend.Get(ctx, storageformat.SuperblockKey())
		if err != nil {
			t.Fatal(err)
		}
		replaceInternalObject(t, backend, storageformat.SuperblockKey(), object.Version, []byte("not-json"))
		if _, err := engine.CreateCheckpoint(ctx, "checkpoint"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("CreateCheckpoint() error = %v", err)
		}
	})

	t.Run("checkpoint-conflict-is-idempotent", func(t *testing.T) {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		engine, err := Open(ctx, options(backend))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.CreateCheckpoint(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.CreateCheckpoint(ctx, "checkpoint"); err != nil {
			t.Fatalf("CreateCheckpoint(retry) error = %v", err)
		}
	})

	t.Run("checkpoint-record-corruption", func(t *testing.T) {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		engine, err := Open(ctx, options(backend))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.CreateCheckpoint(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
		key := storageformat.CheckpointKey("checkpoint")
		object, err := backend.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		version := replaceInternalObject(t, backend, key, object.Version, []byte("not-json"))
		if _, err := engine.readCheckpoint(ctx, "checkpoint"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("readCheckpoint(malformed) error = %v", err)
		}
		invalid := storageformat.Checkpoint{SchemaVersion: 2, CheckpointID: "checkpoint"}
		replaceInternalObject(t, backend, key, version, encodeInternalEnvelope(t, checkpointSchema, key, 1, invalid))
		if _, err := engine.readCheckpoint(ctx, "checkpoint"); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpoint(incompatible) error = %v", err)
		}
	})

	_ = fixtureEngine
}
