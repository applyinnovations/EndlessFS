package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestRunVerifiesLocalRawCopyFixtureWithoutWritingIt(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2041, 1, 2, 3, 4, 5, 0, time.UTC))
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x71}, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "provider-verify"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "fixture-checkpoint"); err != nil {
		t.Fatal(err)
	}

	fixture := make(map[string]string)
	for key, body := range backend.Export() {
		fixture[key] = base64.StdEncoding.EncodeToString(body)
	}
	directory := t.TempDir()
	fixturePath := filepath.Join(directory, "fixture.json")
	writeJSON(t, fixturePath, fixture)
	configPath := filepath.Join(directory, "verify.json")
	writeJSON(t, configPath, verificationConfig{
		Provider: "memory", Fixture: fixturePath, CheckpointID: "fixture-checkpoint",
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
		KeyringIdentifiers: []string{"session-v1"},
	})
	original, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(context.Background(), []string{"check", configPath}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(output.String(), "fixture-checkpoint") {
		t.Fatalf("output = %q", output.String())
	}
	after, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("read-only verification changed fixture")
	}
}

func TestRunVerifiesSeparateStateAndFileFixtures(t *testing.T) {
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2041, 1, 2, 3, 4, 5, 0, time.UTC))
	writer := portable.WriterConfiguration{
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
		KeyringIdentifiers: []string{"session-v1"},
	}
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x72}, 1<<20))), Writer: writer,
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	blobKey := storageformat.BlobKey("U1NTU1NTU1NTU1NTU1NTUw", "provider-verify")
	if _, err := fileBackend.Put(context.Background(), blobKey, []byte("file bytes"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "split-provider-verify"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "split-fixture-checkpoint"); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	filePath := filepath.Join(directory, "files.json")
	writeEncodedFixture(t, statePath, stateBackend.Export())
	writeEncodedFixture(t, filePath, fileBackend.Export())
	configPath := filepath.Join(directory, "verify.json")
	writeJSON(t, configPath, verificationConfig{
		Provider: "memory", Fixture: statePath, FileFixture: filePath, CheckpointID: "split-fixture-checkpoint",
		WriterSetID: writer.WriterSetID, ConfigurationDigest: writer.ConfigurationDigest, KeyringIdentifiers: writer.KeyringIdentifiers,
	})
	if err := run(context.Background(), []string{"check", configPath}, &bytes.Buffer{}); err != nil {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunRejectsMalformedFixtureAndUnknownConfiguration(t *testing.T) {
	directory := t.TempDir()
	fixturePath := filepath.Join(directory, "fixture.json")
	if err := os.WriteFile(fixturePath, []byte(`{"endlessfs/v1/superblock.json":"not base64"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "verify.json")
	if err := os.WriteFile(configPath, []byte(`{"provider":"memory","fixture":"`+fixturePath+`","checkpointID":"fixture-checkpoint","writerSetID":"writer","configurationDigest":"digest","keyringIdentifiers":["key"],"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"check", configPath}, &bytes.Buffer{}); err == nil {
		t.Fatal("run() accepted unknown configuration field")
	}
}

func TestVerificationInputBoundaryMatrix(t *testing.T) {
	for _, args := range [][]string{nil, {"check"}, {"verify", "config.json"}, {"check", ""}, {"check", "a", "b"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("run(%q) error = %v", args, err)
		}
	}
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.json")
	if _, err := readBoundedFile(missing, 10); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing input error = %v", err)
	}
	if err := run(context.Background(), []string{"check", missing}, &bytes.Buffer{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing configuration error = %v", err)
	}
	missingParent := filepath.Join(directory, "missing", "input.json")
	if _, err := readBoundedFile(missingParent, 10); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing input directory error = %v", err)
	}
	empty := filepath.Join(directory, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(empty, 10); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty input error = %v", err)
	}
	large := filepath.Join(directory, "large.json")
	if err := os.WriteFile(large, bytes.Repeat([]byte("x"), 11), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(large, 10); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("large input error = %v", err)
	}
	if _, err := readBoundedFile(directory, maximumVerificationConfigBytes); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("directory input error = %v", err)
	}

	cases := []verificationConfig{
		{},
		{Provider: "unknown", CheckpointID: "checkpoint", WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		{Provider: "memory", CheckpointID: "checkpoint", WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		{Provider: "memory", FileBucket: "forbidden", Fixture: "fixture.json", CheckpointID: "checkpoint", WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		{Provider: "gcs", CheckpointID: "checkpoint", WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		{Provider: "gcs", FileBucket: "bucket", Fixture: "forbidden", CheckpointID: "checkpoint", WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
	}
	for index, configuration := range cases {
		path := filepath.Join(directory, "config-"+string(rune('a'+index))+".json")
		writeJSON(t, path, configuration)
		if err := run(context.Background(), []string{"check", path}, &bytes.Buffer{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("configuration %d error = %v", index, err)
		}
	}

	badFixture := filepath.Join(directory, "bad-fixture.json")
	writeJSON(t, badFixture, map[string]string{"endlessfs/v1/superblock.json": "not-base64"})
	badFixtureConfig := filepath.Join(directory, "bad-fixture-config.json")
	writeJSON(t, badFixtureConfig, verificationConfig{Provider: "memory", Fixture: "bad-fixture.json", CheckpointID: "checkpoint", WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}})
	if err := run(context.Background(), []string{"check", badFixtureConfig}, &bytes.Buffer{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("bad fixture error = %v", err)
	}

	invalidKeyFixture := filepath.Join(directory, "invalid-key-fixture.json")
	writeJSON(t, invalidKeyFixture, map[string]string{"INVALID": base64.StdEncoding.EncodeToString([]byte("body"))})
	invalidKeyConfig := filepath.Join(directory, "invalid-key-config.json")
	writeJSON(t, invalidKeyConfig, verificationConfig{Provider: "memory", Fixture: invalidKeyFixture, CheckpointID: "checkpoint", WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}})
	if err := run(context.Background(), []string{"check", invalidKeyConfig}, &bytes.Buffer{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid fixture key error = %v", err)
	}

	emptyFixture := filepath.Join(directory, "empty-objects.json")
	writeJSON(t, emptyFixture, map[string]string{})
	for name, configuration := range map[string]verificationConfig{
		"missing state fixture": {
			Provider: "memory", Fixture: "absent-state.json", CheckpointID: "checkpoint",
			WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"},
		},
		"missing file fixture": {
			Provider: "memory", Fixture: emptyFixture, FileFixture: "absent-files.json", CheckpointID: "checkpoint",
			WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"},
		},
		"checkpoint absent from fixture": {
			Provider: "memory", Fixture: emptyFixture, CheckpointID: "checkpoint",
			WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-")+".json")
			writeJSON(t, path, configuration)
			if err := run(context.Background(), []string{"check", path}, &bytes.Buffer{}); err == nil {
				t.Fatal("invalid verification input was accepted")
			}
		})
	}
	malformedFixture := filepath.Join(directory, "malformed-objects.json")
	if err := os.WriteFile(malformedFixture, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	malformedFixtureConfig := filepath.Join(directory, "malformed-objects-config.json")
	writeJSON(t, malformedFixtureConfig, verificationConfig{
		Provider: "memory", Fixture: malformedFixture, CheckpointID: "checkpoint",
		WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"},
	})
	if err := run(context.Background(), []string{"check", malformedFixtureConfig}, &bytes.Buffer{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed fixture error = %v", err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeEncodedFixture(t *testing.T, path string, objects map[string][]byte) {
	t.Helper()
	fixture := make(map[string]string, len(objects))
	for key, body := range objects {
		fixture[key] = base64.StdEncoding.EncodeToString(body)
	}
	writeJSON(t, path, fixture)
}
