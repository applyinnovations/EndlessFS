package portable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	sourceFile, err := sourceEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/documents/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
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
	if err := destination.Import(authoritativeCopy(t, sourceEngine, source.Export(), checkpoint)); err != nil {
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
	destinationFile, err := destinationEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/documents/file.txt"))
	if err != nil {
		t.Fatalf("destination original file missing: %v", err)
	}
	if destinationFile.PreviewContentIdentity() != sourceFile.PreviewContentIdentity() {
		t.Fatalf("raw-copy preview identity = %+v, want %+v", destinationFile.PreviewContentIdentity(), sourceFile.PreviewContentIdentity())
	}
	if _, err := destinationEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/copy/file.txt")); err != nil {
		t.Fatalf("destination copied file missing: %v", err)
	}
	if _, err := destinationEngine.Files().Stat(context.Background(), trashScope, domain.MustParseUserPath("/trash-folder/deleted.txt")); err != nil {
		t.Fatalf("destination trash file missing: %v", err)
	}
	for path, expected := range map[string][2]int64{"/": {28, 2}, "/documents": {14, 1}, "/copy": {14, 1}} {
		entry, statErr := destinationEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath(path))
		if statErr != nil || entry.Size != expected[0] || entry.FileCount != expected[1] {
			t.Fatalf("raw-copy aggregate %s = %+v, %v; want %d bytes/%d files", path, entry, statErr, expected[0], expected[1])
		}
	}
	if entry, statErr := destinationEngine.Files().Stat(context.Background(), trashScope, domain.MustParseUserPath("/")); statErr != nil || entry.Size != 11 || entry.FileCount != 1 {
		t.Fatalf("raw-copy trash aggregate = %+v, %v; want 11 bytes/1 file", entry, statErr)
	}
	if _, err := destinationEngine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/after-cutover")}); err != nil {
		t.Fatalf("destination continued mutation error = %v", err)
	}
	uploadPortableFile(t, destinationServer.Client(), destinationEngine.Files(), scope, domain.MustParseUserPath("/after-cutover/new.txt"), []byte("continued"))
	if entry, statErr := destinationEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/")); statErr != nil || entry.Size != 37 || entry.FileCount != 3 {
		t.Fatalf("continued aggregate = %+v, %v; want 37 bytes/3 files", entry, statErr)
	}
	returnCheckpoint, err := destinationEngine.CreateCheckpoint(context.Background(), "return-checkpoint")
	if err != nil {
		t.Fatal(err)
	}

	returned := objectmemory.New()
	if err := returned.Import(authoritativeCopy(t, destinationEngine, destination.Export(), returnCheckpoint)); err != nil {
		t.Fatal(err)
	}
	returnedEngine := openEngine(t, returned, clock, 84, nil)
	if err := returnedEngine.OpenWrites(context.Background(), "return-checkpoint"); err != nil {
		t.Fatal(err)
	}
	if _, err := returnedEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/after-cutover")); err != nil {
		t.Fatalf("reverse-copy continued state missing: %v", err)
	}
	if entry, err := returnedEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/")); err != nil || entry.Size != 37 || entry.FileCount != 3 {
		t.Fatalf("reverse-copy aggregate = %+v, %v; want 37 bytes/3 files", entry, err)
	}
}

func TestCheckpointUsesMetadataWithoutReadingFileBodiesOrWritingPerObjectJournals(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2037, 2, 2, 4, 5, 6, 0, time.UTC))
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(78, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	seed := openEngine(t, backend, clock, 79, nil)
	user, _ := domain.ParseUserID("U1NTU1NTU1NTU1NTU1NTUw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	uploadPortableFile(t, server.Client(), seed.Files(), scope, domain.MustParseUserPath("/large.bin"), []byte("metadata-only-checkpoint"))

	guard := &checkpointMetadataOnlyBackend{Backend: backend}
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: guard, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(80, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "metadata-only"); err != nil {
		t.Fatal(err)
	}
	if guard.fileBodyReads != 0 {
		t.Fatalf("checkpoint attempted %d file body reads", guard.fileBodyReads)
	}
	if guard.workWrites != 0 {
		t.Fatalf("checkpoint wrote %d per-object work records", guard.workWrites)
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
	if err := source.VisitCheckpointObjects(context.Background(), checkpoint.CheckpointID, func(object storageformat.CheckpointObject) error {
		if strings.Contains(object.Key, "/blobs/") {
			fileCopy[object.Key] = append([]byte(nil), sourceFileObjects[object.Key]...)
		} else {
			stateCopy[object.Key] = append([]byte(nil), sourceStateObjects[object.Key]...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkpointKey := storageformat.CheckpointKey(checkpoint.CheckpointID).String()
	stateCopy[checkpointKey] = append([]byte(nil), sourceStateObjects[checkpointKey]...)
	for index := uint64(0); index < checkpoint.InventoryPageCount; index++ {
		pageKey := storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, index).String()
		stateCopy[pageKey] = append([]byte(nil), sourceStateObjects[pageKey]...)
	}
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
	if err != nil || entry.FileCount != 1 {
		t.Fatalf("split raw-copy file = %+v, %v", entry, err)
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

func TestCheckpointSupportsInventoryBeyondCanonicalRecordLimit(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 2, 5, 4, 5, 6, 0, time.UTC))
	engine := openEngine(t, backend, clock, 155, nil)
	objects := make(map[string][]byte, 12_080)
	for index := range 12_080 {
		objects[fmt.Sprintf("endlessfs/v1/test/large-checkpoint/%05d", index)] = []byte{byte(index)}
	}
	if err := backend.Import(objects); err != nil {
		t.Fatal(err)
	}

	checkpoint, err := engine.CreateCheckpoint(context.Background(), "large-checkpoint")
	if err != nil {
		t.Fatalf("CreateCheckpoint() large inventory error = %v", err)
	}
	if checkpoint.SchemaVersion != 3 || checkpoint.StateObjectCount+checkpoint.FileObjectCount < uint64(len(objects)) || checkpoint.InventoryPageCount < 2 {
		t.Fatalf("checkpoint root = %+v; want paged inventory for at least %d objects", checkpoint, len(objects))
	}
	checkpointBody := backend.Export()[storageformat.CheckpointKey(checkpoint.CheckpointID).String()]
	if len(checkpointBody) == 0 || len(checkpointBody) > storageformat.MaxCanonicalBytes {
		t.Fatalf("checkpoint root body size = %d; want bounded non-empty root", len(checkpointBody))
	}
	visited := 0
	if err := engine.VisitCheckpointObjects(context.Background(), checkpoint.CheckpointID, func(storageformat.CheckpointObject) error {
		visited++
		return nil
	}); err != nil {
		t.Fatalf("VisitCheckpointObjects() large inventory error = %v", err)
	}
	if visited != int(checkpoint.StateObjectCount+checkpoint.FileObjectCount) {
		t.Fatalf("visited checkpoint objects = %d; want %d", visited, checkpoint.StateObjectCount+checkpoint.FileObjectCount)
	}
	if err := engine.VerifyCheckpoint(context.Background(), checkpoint.CheckpointID); err != nil {
		t.Fatalf("VerifyCheckpoint() large inventory error = %v", err)
	}
}

func TestCheckpointV1IsRejectedThenReplacedWithoutReadingFileBodies(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 2, 6, 4, 5, 6, 0, time.UTC))
	engine := openEngine(t, backend, clock, 156, nil)
	const checkpointID = "legacy-v1-checkpoint"
	if err := engine.CloseWrites(context.Background(), checkpointID); err != nil {
		t.Fatal(err)
	}
	gateObject, err := backend.Get(context.Background(), storageformat.WriteGateKey())
	if err != nil {
		t.Fatal(err)
	}
	var gateEnvelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(gateObject.Body, storageformat.WriteGateKey(), "write-gate-v1", &gateEnvelope, &gate); err != nil {
		t.Fatal(err)
	}
	superblockObject, err := backend.Get(context.Background(), storageformat.SuperblockKey())
	if err != nil {
		t.Fatal(err)
	}
	var superblock storageformat.Superblock
	if err := state.DecodeJSONWithLimit(superblockObject.Body, &superblock, storageformat.MaxCanonicalBytes); err != nil {
		t.Fatal(err)
	}
	var inventory []storageformat.CheckpointObject
	for key, body := range backend.Export() {
		if !strings.HasPrefix(key, "endlessfs/v1/") || strings.HasPrefix(key, "endlessfs/v1/admissions/") || strings.HasPrefix(key, "endlessfs/v1/staging/") || strings.HasPrefix(key, "endlessfs/v1/leases/") || strings.HasPrefix(key, "endlessfs/v1/checkpoints/") {
			continue
		}
		inventory = append(inventory, storageformat.CheckpointObject{Key: key, Size: int64(len(body)), SHA256: storageformat.Digest(body)})
	}
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Key < inventory[j].Key })
	inventoryBody, err := storageformat.EncodeCanonical(inventory)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := storageformat.Checkpoint{
		SchemaVersion: 1, CheckpointID: checkpointID, BucketID: superblock.BucketID,
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", GateEpoch: gate.Epoch,
		KeyFormatVersion: storageformat.KeyFormatVersion, WriterProtocolVersion: storageformat.WriterProtocolVersion,
		CreatedAt: clock.Now(), Objects: inventory, InventoryDigest: storageformat.Digest(inventoryBody),
	}
	key := storageformat.CheckpointKey(checkpointID)
	body, err := storageformat.EncodeEnvelope("checkpoint-v1", key, 1, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := engine.VerifyCheckpoint(context.Background(), checkpointID); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("VerifyCheckpoint() v1 error = %v; want precondition failed", err)
	}
	replacement, err := engine.CreateCheckpoint(context.Background(), checkpointID)
	if err != nil || replacement.SchemaVersion != 3 {
		t.Fatalf("CreateCheckpoint() replacement = %+v, %v; want checkpoint v3", replacement, err)
	}
	if err := engine.VerifyCheckpoint(context.Background(), checkpointID); err != nil {
		t.Fatalf("VerifyCheckpoint() replacement error = %v", err)
	}
}

func authoritativeCopy(t *testing.T, engine *portable.Engine, source map[string][]byte, checkpoint storageformat.Checkpoint) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	if err := engine.VisitCheckpointObjects(context.Background(), checkpoint.CheckpointID, func(object storageformat.CheckpointObject) error {
		body, found := source[object.Key]
		if !found {
			t.Fatalf("checkpoint object %q is absent from source", object.Key)
		}
		result[object.Key] = append([]byte(nil), body...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkpointKey := storageformat.CheckpointKey(checkpoint.CheckpointID).String()
	body, found := source[checkpointKey]
	if !found {
		t.Fatalf("checkpoint record %q is absent from source", checkpointKey)
	}
	result[checkpointKey] = append([]byte(nil), body...)
	for index := uint64(0); index < checkpoint.InventoryPageCount; index++ {
		pageKey := storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, index).String()
		pageBody, found := source[pageKey]
		if !found {
			t.Fatalf("checkpoint inventory page %q is absent from source", pageKey)
		}
		result[pageKey] = append([]byte(nil), pageBody...)
	}
	return result
}

func checkpointObjects(t *testing.T, engine *portable.Engine, checkpointID string) []storageformat.CheckpointObject {
	t.Helper()
	var objects []storageformat.CheckpointObject
	if err := engine.VisitCheckpointObjects(context.Background(), checkpointID, func(object storageformat.CheckpointObject) error {
		objects = append(objects, object)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return objects
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
		if strings.HasPrefix(objectKey, storageformat.StateIndexRootPrefix()) {
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
	t.Run("missing checkpoint inventory page", func(t *testing.T) {
		objects := cloneObjects(base)
		delete(objects, storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, 0).String())
		assertRejected(t, verify(t, objects, writer, checkpoint.CheckpointID))
	})
	t.Run("malformed checkpoint inventory page", func(t *testing.T) {
		objects := cloneObjects(base)
		objects[storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, 0).String()] = []byte("{")
		assertRejected(t, verify(t, objects, writer, checkpoint.CheckpointID))
	})
	t.Run("checkpoint inventory chain mismatch", func(t *testing.T) {
		objects := cloneObjects(base)
		key := storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, 0)
		var envelope storageformat.Envelope
		var page storageformat.CheckpointInventoryPage
		if err := storageformat.DecodeEnvelope(objects[key.String()], key, "checkpoint-inventory-page-v2", &envelope, &page); err != nil {
			t.Fatal(err)
		}
		page.PreviousDigest = storageformat.Digest([]byte("different predecessor"))
		body, err := storageformat.EncodeEnvelope("checkpoint-inventory-page-v2", key, envelope.Revision, page)
		if err != nil {
			t.Fatal(err)
		}
		objects[key.String()] = body
		assertRejected(t, verify(t, objects, writer, checkpoint.CheckpointID))
	})
	t.Run("extra checkpoint inventory page", func(t *testing.T) {
		objects := cloneObjects(base)
		first := storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, 0).String()
		extra := storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, checkpoint.InventoryPageCount).String()
		objects[extra] = append([]byte(nil), objects[first]...)
		assertRejected(t, verify(t, objects, writer, checkpoint.CheckpointID))
	})
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
		if err := storageformat.DecodeEnvelope(objects[key.String()], key, "checkpoint-v3", &envelope, &stored); err != nil {
			t.Fatal(err)
		}
		stored.SchemaVersion++
		objects[key.String()], err = storageformat.EncodeEnvelope("checkpoint-v3", key, envelope.Revision, stored)
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
	base := authoritativeCopy(t, engine, backend.Export(), checkpoint)
	checkpointInventory := checkpointObjects(t, engine, checkpoint.CheckpointID)
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
		delete(objects, checkpointInventory[len(checkpointInventory)-1].Key)
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

type checkpointMetadataOnlyBackend struct {
	objectstore.Backend
	fileBodyReads int
	workWrites    int
}

func (backend *checkpointMetadataOnlyBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	if strings.Contains(key.String(), "/blobs/") || strings.Contains(key.String(), "/staging/") {
		backend.fileBodyReads++
		return objectstore.ObjectReader{}, domain.NewError(domain.ErrorPreconditionFailed, "file body read denied by test")
	}
	return backend.Backend.Open(ctx, key)
}

func (backend *checkpointMetadataOnlyBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if strings.HasPrefix(key.String(), storageformat.CheckpointWorkPrefix("metadata-only")) {
		backend.workWrites++
	}
	return backend.Backend.Put(ctx, key, body, condition)
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
