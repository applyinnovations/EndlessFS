package portable_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestPortabilityRawCopyPreservesCompleteStateAndContinuesInBothDirections(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2037, 2, 3, 4, 5, 6, 0, time.UTC))
	source := objectmemory.New()
	sourceServer := httptest.NewServer(source)
	t.Cleanup(sourceServer.Close)
	if err := source.ConfigureDataPlane(sourceServer.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(80, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	sourceEngine := openEngine(t, source, clock, 81, nil)
	stateValues := map[state.Key][]byte{
		state.MustKey(state.NamespaceUsers, "portable-user"):                   []byte(`{"displayName":"Portable"}`),
		state.MustKey(state.NamespaceCredentials, "portable-credential"):       []byte(`{"credential":"portable"}`),
		state.MustKey(state.NamespaceSessions, "portable-session"):             []byte(`{"session":"portable"}`),
		state.MustKey(state.NamespaceRoles, "admins"):                          []byte(`{"admins":["portable-user"]}`),
		state.MustKey(state.NamespaceShares, "portable-share-token-hash"):      []byte(`{"share":"portable"}`),
		state.MustKey(state.NamespaceTrash, "portable-user", "portable-trash"): []byte(`{"trash":"portable"}`),
		state.MustKey(state.NamespacePreferences, "portable-user", "theme"):    []byte(`{"themeID":"endlessfs-dark"}`),
		state.MustKey(state.NamespaceOperations, "portable-operation"):         []byte(`{"state":"succeeded"}`),
		state.MustKey(state.NamespaceIdempotency, "portable-idempotency"):      []byte(`{"outcome":"portable"}`),
	}
	stateVersions := make(map[state.Key]state.Version, len(stateValues))
	for key, value := range stateValues {
		version, createErr := sourceEngine.Create(context.Background(), key, value)
		if createErr != nil {
			t.Fatal(createErr)
		}
		stateVersions[key] = version
	}
	user, _ := domain.ParseUserID("U1NTU1NTU1NTU1NTU1NTUw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	trashScope, _ := domain.NewScope(user, domain.AreaTrash)
	if _, err := sourceEngine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/documents")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, sourceServer.Client(), sourceEngine.Files(), scope, domain.MustParseUserPath("/documents/file.txt"), []byte("portable bytes"))
	if _, err := sourceEngine.Files().CreateDirectory(context.Background(), trashScope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/trash-folder")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, sourceServer.Client(), sourceEngine.Files(), trashScope, domain.MustParseUserPath("/trash-folder/deleted.txt"), []byte("trash bytes"))
	if _, err := sourceEngine.Files().Copy(context.Background(), scope, scope, domain.CopyRequest{Source: domain.MustParseUserPath("/documents"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "portable-copy"}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := sourceEngine.CreateCheckpoint(context.Background(), "source-checkpoint")
	if err != nil {
		t.Fatal(err)
	}

	destination := objectmemory.New()
	destinationServer := httptest.NewServer(destination)
	t.Cleanup(destinationServer.Close)
	if err := destination.ConfigureDataPlane(destinationServer.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(82, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	if err := destination.Import(authoritativeCopy(t, source.Export(), checkpoint)); err != nil {
		t.Fatal(err)
	}
	destinationEngine := openEngine(t, destination, clock, 83, nil)
	if err := destinationEngine.VerifyCheckpoint(context.Background(), "source-checkpoint"); err != nil {
		t.Fatal(err)
	}
	if err := destinationEngine.OpenWrites(context.Background(), "source-checkpoint"); err != nil {
		t.Fatal(err)
	}
	for key, expected := range stateValues {
		value, getErr := destinationEngine.Get(context.Background(), key)
		if getErr != nil || value.Version != stateVersions[key] || !bytes.Equal(value.Data, expected) {
			t.Fatalf("destination state %q = %+v, %v", key, value, getErr)
		}
	}
	if _, err := destinationEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/copy/file.txt")); err != nil {
		t.Fatalf("destination copied file missing: %v", err)
	}
	if _, err := destinationEngine.Files().Stat(context.Background(), trashScope, domain.MustParseUserPath("/trash-folder/deleted.txt")); err != nil {
		t.Fatalf("destination trash file missing: %v", err)
	}
	if _, err := destinationEngine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/after-cutover")}); err != nil {
		t.Fatalf("destination continued mutation error = %v", err)
	}
	returnCheckpoint, err := destinationEngine.CreateCheckpoint(context.Background(), "return-checkpoint")
	if err != nil {
		t.Fatal(err)
	}

	returned := objectmemory.New()
	if err := returned.Import(authoritativeCopy(t, destination.Export(), returnCheckpoint)); err != nil {
		t.Fatal(err)
	}
	returnedEngine := openEngine(t, returned, clock, 84, nil)
	if err := returnedEngine.OpenWrites(context.Background(), "return-checkpoint"); err != nil {
		t.Fatal(err)
	}
	if _, err := returnedEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/after-cutover")); err != nil {
		t.Fatalf("reverse-copy continued state missing: %v", err)
	}
}

func TestPortabilityRawCopyPreservesSplitStateAndFileBackends(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2037, 2, 4, 4, 5, 6, 0, time.UTC))
	writer := portable.WriterConfiguration{
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
		KeyringIdentifiers: []string{"session-v1"},
	}
	sourceState := objectmemory.New()
	sourceFiles := objectmemory.New()
	sourceServer := httptest.NewServer(sourceFiles)
	t.Cleanup(sourceServer.Close)
	if err := sourceFiles.ConfigureDataPlane(sourceServer.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(151, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	source, err := portable.Open(context.Background(), portable.Options{
		Backend: sourceState, FileBackend: sourceFiles, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(152, 1<<20))), Writer: writer,
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	stateKey := state.MustKey(state.NamespaceAccounts, "split-portability")
	if _, err := source.Create(context.Background(), stateKey, []byte("portable state")); err != nil {
		t.Fatal(err)
	}
	user, _ := domain.ParseUserID("VFRUVFRUVFRUVFRUVFRUVA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/portable.txt")
	content := []byte("portable split bytes")
	uploadPortableFile(t, sourceServer.Client(), source.Files(), scope, path, content)
	checkpoint, err := source.CreateCheckpoint(context.Background(), "split-portability")
	if err != nil {
		t.Fatal(err)
	}

	destinationState := objectmemory.New()
	destinationFiles := objectmemory.New()
	stateCopy := make(map[string][]byte)
	fileCopy := make(map[string][]byte)
	sourceStateObjects := sourceState.Export()
	sourceFileObjects := sourceFiles.Export()
	for _, object := range checkpoint.Objects {
		if strings.Contains(object.Key, "/blobs/") {
			fileCopy[object.Key] = append([]byte(nil), sourceFileObjects[object.Key]...)
		} else {
			stateCopy[object.Key] = append([]byte(nil), sourceStateObjects[object.Key]...)
		}
	}
	checkpointKey := storageformat.CheckpointKey(checkpoint.CheckpointID).String()
	stateCopy[checkpointKey] = append([]byte(nil), sourceStateObjects[checkpointKey]...)
	if err := destinationState.Import(stateCopy); err != nil {
		t.Fatal(err)
	}
	if err := destinationFiles.Import(fileCopy); err != nil {
		t.Fatal(err)
	}
	destinationServer := httptest.NewServer(destinationFiles)
	t.Cleanup(destinationServer.Close)
	if err := destinationFiles.ConfigureDataPlane(destinationServer.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(153, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	destination, err := portable.Open(context.Background(), portable.Options{
		Backend: destinationState, FileBackend: destinationFiles, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(154, 1<<20))), Writer: writer,
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.OpenWrites(context.Background(), checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if value, err := destination.Get(context.Background(), stateKey); err != nil || string(value.Data) != "portable state" {
		t.Fatalf("destination state = %+v, %v", value, err)
	}
	entry, err := destination.Files().Stat(context.Background(), scope, path)
	if err != nil {
		t.Fatal(err)
	}
	download, err := destination.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(download.Method, download.URL, nil)
	response, err := destinationServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, content) {
		t.Fatalf("destination download = %d %q", response.StatusCode, downloaded)
	}
	if _, err := destination.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/after-cutover")}); err != nil {
		t.Fatalf("post-cutover mutation error = %v", err)
	}
}

func authoritativeCopy(t *testing.T, source map[string][]byte, checkpoint storageformat.Checkpoint) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte, len(checkpoint.Objects)+1)
	for _, object := range checkpoint.Objects {
		body, found := source[object.Key]
		if !found {
			t.Fatalf("checkpoint object %q is absent from source", object.Key)
		}
		result[object.Key] = append([]byte(nil), body...)
	}
	checkpointKey := storageformat.CheckpointKey(checkpoint.CheckpointID).String()
	body, found := source[checkpointKey]
	if !found {
		t.Fatalf("checkpoint record %q is absent from source", checkpointKey)
	}
	result[checkpointKey] = append([]byte(nil), body...)
	return result
}

func TestCheckpointDetectsAuthoritativeCorruption(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 1, 1, 0, 0, 0, 0, time.UTC))
	engine := openEngine(t, backend, clock, 21, nil)
	key := state.MustKey(state.NamespaceUsers, "checkpoint-user")
	if _, err := engine.Create(context.Background(), key, []byte("valid")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "checkpoint-corruption"); err != nil {
		t.Fatal(err)
	}
	if err := engine.VerifyCheckpoint(context.Background(), "checkpoint-corruption"); err != nil {
		t.Fatalf("VerifyCheckpoint() error = %v", err)
	}
	objects := backend.Export()
	var target string
	for objectKey := range objects {
		if len(objectKey) > len("endlessfs/v1/state/") && objectKey[:len("endlessfs/v1/state/")] == "endlessfs/v1/state/" {
			target = objectKey
			break
		}
	}
	if target == "" {
		t.Fatal("state object not found")
	}
	parsed := objectstore.MustKey(target)
	current, _ := backend.Get(context.Background(), parsed)
	if _, err := backend.Put(context.Background(), parsed, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: current.Version}); err != nil {
		t.Fatal(err)
	}
	if err := engine.VerifyCheckpoint(context.Background(), "checkpoint-corruption"); err == nil {
		t.Fatal("VerifyCheckpoint() accepted corrupt authoritative object")
	}
}

func TestCheckpointVerifierIsStrictlyReadOnly(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 3, 4, 5, 6, 7, 0, time.UTC))
	engine := openEngine(t, backend, clock, 85, nil)
	if _, err := engine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "verify-only"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "verify-only"); err != nil {
		t.Fatal(err)
	}
	guard := &readOnlyBackend{Backend: backend}
	err := portable.VerifyCheckpointReadOnly(context.Background(), guard, portable.WriterConfiguration{
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
		KeyringIdentifiers: []string{"session-v1"},
	}, "verify-only")
	if err != nil {
		t.Fatalf("VerifyCheckpointReadOnly() error = %v", err)
	}
	if guard.writes != 0 {
		t.Fatalf("read-only verifier attempted %d writes", guard.writes)
	}
}

func TestCheckpointVerifierRejectsInvalidBootstrapAndCheckpointRecords(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 3, 4, 6, 7, 8, 0, time.UTC))
	engine := openEngine(t, backend, clock, 155, nil)
	checkpoint, err := engine.CreateCheckpoint(context.Background(), "verify-boundaries")
	if err != nil {
		t.Fatal(err)
	}
	base := backend.Export()
	writer := portable.WriterConfiguration{
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
		KeyringIdentifiers: []string{"session-v1"},
	}
	verify := func(t *testing.T, objects map[string][]byte, configuration portable.WriterConfiguration, checkpointID string) error {
		t.Helper()
		destination := objectmemory.New()
		if err := destination.Import(objects); err != nil {
			t.Fatal(err)
		}
		return portable.VerifyCheckpointReadOnly(context.Background(), destination, configuration, checkpointID)
	}
	assertRejected := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, domain.ErrInvalid) && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verification error = %v", err)
		}
	}

	t.Run("nil backend", func(t *testing.T) {
		assertRejected(t, portable.VerifyCheckpointReadOnly(context.Background(), nil, writer, checkpoint.CheckpointID))
	})
	t.Run("invalid writer configuration", func(t *testing.T) {
		assertRejected(t, verify(t, base, portable.WriterConfiguration{}, checkpoint.CheckpointID))
	})
	for name, key := range map[string]objectstore.Key{
		"missing superblock": storageformat.SuperblockKey(),
		"missing writer set": storageformat.WriterSetKey(),
		"missing write gate": storageformat.WriteGateKey(),
		"missing checkpoint": storageformat.CheckpointKey(checkpoint.CheckpointID),
	} {
		t.Run(name, func(t *testing.T) {
			objects := cloneObjects(base)
			delete(objects, key.String())
			assertRejected(t, verify(t, objects, writer, checkpoint.CheckpointID))
		})
	}
	for name, key := range map[string]objectstore.Key{
		"malformed superblock": storageformat.SuperblockKey(),
		"malformed writer set": storageformat.WriterSetKey(),
		"malformed write gate": storageformat.WriteGateKey(),
		"malformed checkpoint": storageformat.CheckpointKey(checkpoint.CheckpointID),
	} {
		t.Run(name, func(t *testing.T) {
			objects := cloneObjects(base)
			objects[key.String()] = []byte("{")
			assertRejected(t, verify(t, objects, writer, checkpoint.CheckpointID))
		})
	}
	t.Run("incompatible writer set", func(t *testing.T) {
		configuration := writer
		configuration.ConfigurationDigest = "different-config"
		assertRejected(t, verify(t, base, configuration, checkpoint.CheckpointID))
	})
	t.Run("incompatible checkpoint", func(t *testing.T) {
		objects := cloneObjects(base)
		key := storageformat.CheckpointKey(checkpoint.CheckpointID)
		var envelope storageformat.Envelope
		var stored storageformat.Checkpoint
		if err := storageformat.DecodeEnvelope(objects[key.String()], key, "checkpoint-v1", &envelope, &stored); err != nil {
			t.Fatal(err)
		}
		stored.SchemaVersion++
		objects[key.String()], err = storageformat.EncodeEnvelope("checkpoint-v1", key, envelope.Revision, stored)
		if err != nil {
			t.Fatal(err)
		}
		assertRejected(t, verify(t, objects, writer, checkpoint.CheckpointID))
	})
	t.Run("checkpoint does not match closed gate", func(t *testing.T) {
		key := storageformat.WriteGateKey()
		original, getErr := backend.Get(context.Background(), key)
		if getErr != nil {
			t.Fatal(getErr)
		}
		var envelope storageformat.Envelope
		var gate storageformat.WriteGate
		if err := storageformat.DecodeEnvelope(original.Body, key, "write-gate-v1", &envelope, &gate); err != nil {
			t.Fatal(err)
		}
		gate.CheckpointID = "different-checkpoint"
		body, encodeErr := storageformat.EncodeEnvelope("write-gate-v1", key, envelope.Revision+1, gate)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		version, putErr := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: original.Version})
		if putErr != nil {
			t.Fatal(putErr)
		}
		assertRejected(t, engine.VerifyCheckpoint(context.Background(), checkpoint.CheckpointID))
		if _, putErr := backend.Put(context.Background(), key, original.Body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); putErr != nil {
			t.Fatal(putErr)
		}
	})
	t.Run("malformed superblock during checkpoint retry", func(t *testing.T) {
		key := storageformat.SuperblockKey()
		original, getErr := backend.Get(context.Background(), key)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if _, putErr := backend.Put(context.Background(), key, []byte("{"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: original.Version}); putErr != nil {
			t.Fatal(putErr)
		}
		if _, createErr := engine.CreateCheckpoint(context.Background(), checkpoint.CheckpointID); !errors.Is(createErr, domain.ErrInvalid) {
			t.Fatalf("CreateCheckpoint() error = %v", createErr)
		}
	})
	t.Run("empty checkpoint ID", func(t *testing.T) {
		assertRejected(t, engine.VerifyCheckpoint(context.Background(), ""))
	})
}

func TestCheckpointVerifierRejectsMissingExtraAndUnsupportedState(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 3, 5, 5, 6, 7, 0, time.UTC))
	engine := openEngine(t, backend, clock, 86, nil)
	if _, err := engine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "verification-matrix"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := engine.CreateCheckpoint(context.Background(), "verification-matrix")
	if err != nil {
		t.Fatal(err)
	}
	base := authoritativeCopy(t, backend.Export(), checkpoint)
	writer := portable.WriterConfiguration{
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
		KeyringIdentifiers: []string{"session-v1"},
	}
	verify := func(objects map[string][]byte) error {
		destination := objectmemory.New()
		if err := destination.Import(objects); err != nil {
			t.Fatal(err)
		}
		return portable.VerifyCheckpointReadOnly(context.Background(), destination, writer, checkpoint.CheckpointID)
	}
	t.Run("missing authoritative object", func(t *testing.T) {
		objects := cloneObjects(base)
		delete(objects, checkpoint.Objects[len(checkpoint.Objects)-1].Key)
		if err := verify(objects); !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("verification error = %v", err)
		}
	})
	t.Run("extra authoritative object", func(t *testing.T) {
		objects := cloneObjects(base)
		objects[storageformat.StateKey("users", state.MustKey(state.NamespaceUsers, "extra").String()).String()] = []byte("extra")
		if err := verify(objects); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verification error = %v", err)
		}
	})
	t.Run("unsupported superblock feature", func(t *testing.T) {
		objects := cloneObjects(base)
		var superblock storageformat.Superblock
		if err := state.DecodeJSONWithLimit(objects[storageformat.SuperblockKey().String()], &superblock, storageformat.MaxCanonicalBytes); err != nil {
			t.Fatal(err)
		}
		superblock.RequiredFeatures = []string{"future-incompatible-feature"}
		body, err := storageformat.EncodeCanonical(superblock)
		if err != nil {
			t.Fatal(err)
		}
		objects[storageformat.SuperblockKey().String()] = body
		if err := verify(objects); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verification error = %v", err)
		}
	})
}

func TestCheckpointPrunesExpiredStateSnapshotsButKeepsCurrentVersions(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 3, 6, 5, 6, 7, 0, time.UTC))
	engine := openEngine(t, backend, clock, 87, nil)
	kept := state.MustKey(state.NamespaceAccounts, "kept")
	version, err := engine.Create(context.Background(), kept, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CompareAndSwap(context.Background(), kept, version, []byte("second")); err != nil {
		t.Fatal(err)
	}
	removed := state.MustKey(state.NamespaceSessions, "removed")
	version, err = engine.Create(context.Background(), removed, []byte("sensitive"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Delete(context.Background(), removed, version); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "snapshot-pruning"); err != nil {
		t.Fatal(err)
	}
	page, err := backend.List(context.Background(), objectstore.ListRequest{Prefix: storageformat.StateVersionsPrefix(), Limit: 1000})
	if err != nil || len(page.Objects) != 1 || page.NextCursor != "" {
		t.Fatalf("state-version objects = %+v, %v", page, err)
	}
}

func cloneObjects(source map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(source))
	for key, body := range source {
		result[key] = append([]byte(nil), body...)
	}
	return result
}

type readOnlyBackend struct {
	objectstore.Backend
	writes int
}

func (backend *readOnlyBackend) Put(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
	backend.writes++
	return "", domain.NewError(domain.ErrorInternal, "verifier attempted put")
}

func (backend *readOnlyBackend) Delete(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
	backend.writes++
	return domain.NewError(domain.ErrorInternal, "verifier attempted delete")
}

func (backend *readOnlyBackend) Copy(context.Context, objectstore.Key, objectstore.Key, objectstore.CopyCondition) (objectstore.CopyResult, error) {
	backend.writes++
	return objectstore.CopyResult{}, domain.NewError(domain.ErrorInternal, "verifier attempted copy")
}
