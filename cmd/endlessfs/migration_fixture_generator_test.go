package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const migrationFixtureProducerCommitEnvironment = "ENDLESSFS_MIGRATION_FIXTURE_PRODUCER_COMMIT"

// TestGenerateSchema010MigrationFixtures is invoked only through the Nix
// fixture-generation app after the epoch writer has been committed. Ordinary
// tests skip it and never mutate the checkout.
func TestGenerateSchema010MigrationFixtures(t *testing.T) {
	commit := os.Getenv(migrationFixtureProducerCommitEnvironment)
	if commit == "" {
		t.Skip("schema fixture generation was not requested")
	}
	if len(commit) != 40 {
		t.Fatal("fixture producer commit must be a full Git object ID")
	}
	profiles := []struct {
		name   string
		writer func(*testing.T) portable.WriterConfiguration
	}{
		{name: "portable-minimal", writer: func(*testing.T) portable.WriterConfiguration {
			return portable.WriterConfiguration{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"session-v1"}}
		}},
		{name: "application-disabled", writer: func(t *testing.T) portable.WriterConfiguration {
			writer, err := buildWriterConfiguration(runtimeTestConfig(t), "session-keyring-v1")
			if err != nil {
				t.Fatal(err)
			}
			return writer
		}},
		{name: "application-gcs", writer: func(t *testing.T) portable.WriterConfiguration {
			cfg := runtimeTestConfig(t)
			configureSchema005PreviewProfile(&cfg)
			writer, err := buildWriterConfiguration(cfg, "session-keyring-v1")
			if err != nil {
				t.Fatal(err)
			}
			return writer
		}},
	}
	for index, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			fixture := buildSchema010MigrationFixture(t, commit, byte(0x91+index*17), profile.writer(t))
			body, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("..", "..", "internal", "portable", "testdata", "migrations", "schema-010-"+profile.name+".json")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
	generateSchema010CompleteMigrationFixture(t, commit)
}

// generateSchema010CompleteMigrationFixture migrates the immutable
// predecessor-produced production residue through the real current writer. It
// deliberately preserves the independent semantic oracle instead of
// reconstructing application records with current code.
func generateSchema010CompleteMigrationFixture(t *testing.T, commit string) {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "portable", "testdata", "migrations", "schema-009-v0.4.0-application-complete-residue.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var predecessor applicationMigrationFixture
	if err := decoder.Decode(&predecessor); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("complete predecessor fixture trailing JSON: %v", err)
	}
	if len(predecessor.SemanticOracle) == 0 {
		t.Fatal("complete predecessor fixture has no independent semantic oracle")
	}

	stateBackend, fileBackend := objectmemory.New(), objectmemory.New()
	if err := stateBackend.Import(predecessor.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(predecessor.FileObjects); err != nil {
		t.Fatal(err)
	}
	writer, err := buildWriterConfiguration(runtimeTestConfig(t), "session-keyring-v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := portable.Open(context.Background(), portable.Options{
		Backend: stateBackend, FileBackend: fileBackend,
		Clock: domain.NewFixedClock(predecessor.CreatedAt.Add(time.Hour)),
		IDs:   domain.NewIDGenerator(bytes.NewReader(applicationMigrationBytes(0xd1, 8<<20))), Writer: writer,
		LeaseTTL: time.Minute, UploadTTL: 5 * time.Minute, DownloadTTL: time.Minute,
		CursorKey: bytes.Repeat([]byte{0x63}, 32),
	}); err != nil {
		t.Fatalf("migrate complete schema-009 residue: %v", err)
	}
	fixture := applicationMigrationFixture{
		SchemaVersion: 1, SourceRelease: "schema-010", SourceCommit: commit,
		CreatedAt: predecessor.CreatedAt, UserID: predecessor.UserID,
		StateObjects: stateBackend.Export(), FileObjects: fileBackend.Export(),
		SemanticOracle: predecessor.SemanticOracle,
	}
	body, err = json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join("..", "..", "internal", "portable", "testdata", "migrations", "schema-010-application-complete.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func configureSchema005PreviewProfile(cfg *config.Config) {
	cfg.PreviewProvider = "gcs"
	cfg.GCSPreviewBucket = "migration-preview-bucket"
	cfg.PreviewFormats = []string{"image"}
	cfg.PreviewResolutions = []int{256, 512, 1600}
	cfg.PreviewKeySecret = secret.Value(base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("p", 32))))
}

func buildSchema010MigrationFixture(t *testing.T, commit string, seed byte, writer portable.WriterConfiguration) applicationMigrationFixture {
	t.Helper()
	ctx := context.Background()
	createdAt := time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := domain.NewFixedClock(createdAt)
	stateBackend, fileBackend := objectmemory.New(), objectmemory.New()
	server := httptest.NewServer(fileBackend)
	t.Cleanup(server.Close)
	if err := fileBackend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(applicationMigrationBytes(seed+1, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine, err := portable.Open(ctx, portable.Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(applicationMigrationBytes(seed, 4<<20))), Writer: writer,
		LeaseTTL: time.Minute, UploadTTL: 5 * time.Minute, DownloadTTL: time.Minute,
		CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	if err != nil {
		t.Fatal(err)
	}
	live, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/projects", "/project-a", "/project-a/empty", "/parent", "/parent/project-a", "/parent/project-a/empty"} {
		if _, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatalf("create fixture directory %s: %v", path, err)
		}
	}
	for _, upload := range []struct {
		path string
		body []byte
	}{
		{path: "/project-a/data.txt", body: []byte("duplicate")},
		{path: "/parent/project-a/data.txt", body: []byte("duplicate")},
		{path: "/zero.bin", body: nil},
		{path: "/trash-me.txt", body: []byte("trash")},
	} {
		uploadCurrentSchemaFixtureFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath(upload.path), upload.body)
	}
	trashEntry, err := engine.Files().Stat(ctx, live, domain.MustParseUserPath("/trash-me.txt"))
	if err != nil {
		t.Fatal(err)
	}
	trash, _ := domain.NewScope(user, domain.AreaTrash)
	if _, err := engine.Files().Move(ctx, live, trash, domain.MoveRequest{Source: domain.MustParseUserPath("/trash-me.txt"), Destination: domain.MustParseUserPath("/trash-me.txt"), ExpectedSource: trashEntry.Version, IdempotencyKey: "fixture-trash"}); err != nil {
		t.Fatal(err)
	}
	aborted, err := engine.Files().CreateUpload(ctx, live, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/aborted.bin"), Size: 3, MediaType: "application/octet-stream", Conflict: domain.ConflictFail, IdempotencyKey: "fixture-aborted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Files().AbortUpload(ctx, live, aborted.UploadID); err != nil {
		t.Fatal(err)
	}
	preference := stateKeyForFixture(t, "preferences", "fixture")
	if _, err := engine.Create(ctx, preference, []byte("enabled")); err != nil {
		t.Fatal(err)
	}
	return applicationMigrationFixture{
		SchemaVersion: 1, SourceRelease: "schema-010", SourceCommit: commit, CreatedAt: createdAt,
		UserID: user.String(), StateObjects: stateBackend.Export(), FileObjects: fileBackend.Export(),
	}
}

func stateKeyForFixture(t *testing.T, namespace, part string) state.Key {
	t.Helper()
	key, err := state.NewKey(state.Namespace(namespace), part)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func uploadCurrentSchemaFixtureFile(t *testing.T, client *http.Client, files *portable.FileStore, scope domain.Scope, path domain.UserPath, body []byte) {
	t.Helper()
	capability, err := files.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "application/octet-stream", Conflict: domain.ConflictFail, IdempotencyKey: "fixture-upload-" + path.Name() + fmt.Sprint(len(path.String()))})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("fixture upload status = %d", response.StatusCode)
	}
	if _, err := files.CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
}
