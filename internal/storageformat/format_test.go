package storageformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type failingCanonicalValue struct{}

func (failingCanonicalValue) MarshalJSON() ([]byte, error) { return nil, errors.New("encode failure") }

type decodeThenFailCanonicalValue struct{}

func (*decodeThenFailCanonicalValue) UnmarshalJSON([]byte) error { return nil }
func (decodeThenFailCanonicalValue) MarshalJSON() ([]byte, error) {
	return nil, errors.New("encode failure")
}

type v1DirectoryEntryWithoutPreviewFields struct {
	Name           string           `json:"name"`
	NameDigest     string           `json:"nameDigest"`
	Kind           domain.EntryKind `json:"kind"`
	DirectoryID    string           `json:"directoryID,omitempty"`
	BlobID         string           `json:"blobID,omitempty"`
	Size           int64            `json:"size"`
	MediaType      string           `json:"mediaType,omitempty"`
	SHA256         string           `json:"sha256,omitempty"`
	ModifiedAt     time.Time        `json:"modifiedAt"`
	LogicalVersion string           `json:"logicalVersion"`
}

type v1DirectoryPageWithoutPreviewFields struct {
	SchemaVersion int                                    `json:"schemaVersion"`
	DirectoryID   string                                 `json:"directoryID"`
	PageID        string                                 `json:"pageID"`
	Entries       []v1DirectoryEntryWithoutPreviewFields `json:"entries"`
}

func TestCanonicalEnvelopeAndLogicalVersion(t *testing.T) {
	key := objectstore.MustKey("endlessfs/v1/control/write-gate.json")
	payload := WriteGate{SchemaVersion: 1, Epoch: 7, Mode: GateOpen}
	first, err := EncodeEnvelope("write-gate-v1", key, 3, payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeEnvelope("write-gate-v1", key, 3, payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || strings.Contains(string(first), "\n") {
		t.Fatalf("encoding is not canonical:\n%s\n%s", first, second)
	}
	var decoded Envelope
	if err := DecodeEnvelope(first, key, "write-gate-v1", &decoded, &payload); err != nil {
		t.Fatal(err)
	}
	if decoded.Revision != 3 || decoded.LogicalVersion == "" {
		t.Fatalf("decoded envelope = %+v", decoded)
	}
	changed, _ := EncodeEnvelope("write-gate-v1", key, 4, payload)
	if string(changed) == string(first) {
		t.Fatal("revision change did not change canonical envelope")
	}
}

func TestCanonicalDirectoryRootCarriesRecursiveByteTransition(t *testing.T) {
	root := DirectoryRoot{
		SchemaVersion: 1, DirectoryID: RootDirectoryID, ManifestID: "manifest", RecursiveBytes: 42,
		Pending: &DirectoryTransition{
			OperationID: "operation", Fence: 3, PreManifestID: "manifest", PostManifestID: "next", PostRecursiveBytes: 84,
		},
	}
	body, err := EncodeCanonical(root)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"directoryID":"root","manifestID":"manifest","recursiveBytes":42,"pending":{"operationID":"operation","fence":3,"preManifestID":"manifest","postManifestID":"next","postRecursiveBytes":84}}`
	if string(body) != want {
		t.Fatalf("directory root = %s; want %s", body, want)
	}
	if FeatureRecursiveBytes != "recursive-byte-aggregates-v1" {
		t.Fatalf("recursive-byte feature = %q", FeatureRecursiveBytes)
	}
	manifest := DirectoryManifest{
		SchemaVersion: 1, DirectoryID: RootDirectoryID, ManifestID: "manifest", PageIDs: []string{"page"},
		EntryCount: 2, RecursiveBytes: 42, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	body, err = EncodeCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"schemaVersion":1,"directoryID":"root","manifestID":"manifest","pageIDs":["page"],"entryCount":2,"recursiveBytes":42,"createdAt":"2026-01-02T03:04:05Z"}`
	if string(body) != want {
		t.Fatalf("directory manifest = %s; want %s", body, want)
	}
}

func TestCanonicalDirectoryRootKeyParsing(t *testing.T) {
	userID := "YWFhYWFhYWFhYWFhYWFhYQ"
	for _, directoryID := range []string{RootDirectoryID, "YmJiYmJiYmJiYmJiYmJiYg"} {
		key := DirectoryRootKey(userID, "live", directoryID)
		gotUser, gotArea, gotDirectory, matched, err := ParseDirectoryRootKey(key)
		if err != nil || !matched || gotUser != userID || gotArea != "live" || gotDirectory != directoryID {
			t.Fatalf("ParseDirectoryRootKey(%s) = %q, %q, %q, %t, %v", key, gotUser, gotArea, gotDirectory, matched, err)
		}
	}
	if _, _, _, matched, err := ParseDirectoryRootKey(DirectoryManifestKey(userID, "live", RootDirectoryID, "manifest")); err != nil || matched {
		t.Fatalf("manifest root parse = %t, %v", matched, err)
	}
	valid := DirectoryRootKey(userID, "live", RootDirectoryID).String()
	invalid := []string{
		strings.Replace(valid, encodedPart(userID), "0", 1),
		strings.Replace(valid, "/live/", "/other/", 1),
		strings.Replace(valid, encodedPart(RootDirectoryID), encodedPart("short"), 1),
		strings.Replace(valid, encodedPart(userID), encodedPart("short"), 1),
	}
	for _, value := range invalid {
		key := objectstore.MustKey(value)
		if _, _, _, matched, err := ParseDirectoryRootKey(key); !matched || !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("ParseDirectoryRootKey(%s) = matched %t, error %v", value, matched, err)
		}
	}
	if FilesystemPrefix() != "endlessfs/v1/fs/" {
		t.Fatalf("filesystem prefix = %q", FilesystemPrefix())
	}
}

func TestWriteGateFeatureBindingValidation(t *testing.T) {
	valid := WriteGate{SchemaVersion: 1, Epoch: 2, Mode: GateOpen, WriterFeatures: []string{"feature-a", FeatureRecursiveBytes}}
	if err := ValidateGate(valid); err != nil {
		t.Fatalf("ValidateGate(valid) error = %v", err)
	}
	for _, features := range [][]string{{"feature", "feature"}, {FeatureRecursiveBytes, "feature-a"}} {
		candidate := valid
		candidate.WriterFeatures = features
		if err := ValidateGate(candidate); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("ValidateGate(%v) error = %v", features, err)
		}
	}
}

func TestCanonicalDirectoryPageRemainsReadableWithoutFormatMigration(t *testing.T) {
	key := DirectoryPageKey("AAAAAAAAAAAAAAAAAAAAAA", "live", RootDirectoryID, "legacy-page")
	payload := v1DirectoryPageWithoutPreviewFields{
		SchemaVersion: 1,
		DirectoryID:   RootDirectoryID,
		PageID:        "legacy-page",
		Entries: []v1DirectoryEntryWithoutPreviewFields{{
			Name: "image.png", NameDigest: NameDigest("image.png"), Kind: domain.EntryFile,
			BlobID: "blob-id", Size: 4, MediaType: "image/png", SHA256: "digest",
			ModifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), LogicalVersion: "version",
		}},
	}
	body, err := EncodeEnvelope("directory-page-v1", key, 1, payload)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	var decoded DirectoryPage
	if err := DecodeEnvelope(body, key, "directory-page-v1", &envelope, &decoded); err != nil {
		t.Fatalf("pre-preview canonical directory page became unreadable: %v", err)
	}
	if len(decoded.Entries) != 1 || decoded.Entries[0].BlobID != "blob-id" {
		t.Fatalf("decoded directory page = %+v", decoded)
	}
}

func TestCanonicalKeyLayoutAndBounds(t *testing.T) {
	for name, key := range map[string]objectstore.Key{
		"superblock":  SuperblockKey(),
		"writer set":  WriterSetKey(),
		"gate":        WriteGateKey(),
		"state":       StateKey("sessions", "sessions/dXNlcg/aWQ"),
		"state view":  StateVersionKey("sessions", "sessions/dXNlcg/aWQ", "logical-version"),
		"admission":   AdmissionKey(42, "operation-id"),
		"staging":     StagingKey("user-id", "operation-id", "artifact-id"),
		"blob":        BlobKey("user-id", "blob-id"),
		"directory":   DirectoryRootKey("user-id", "live", RootDirectoryID),
		"manifest":    DirectoryManifestKey("user-id", "live", RootDirectoryID, "manifest-id"),
		"page":        DirectoryPageKey("user-id", "live", RootDirectoryID, "page-id"),
		"operation":   OperationKey("user-id", "operation-id"),
		"idempotency": IdempotencyKey("user-id", "request-key"),
		"checkpoint":  CheckpointKey("checkpoint-id"),
		"lease":       LeaseKey("gcs", "lease-id"),
	} {
		if !key.Valid() || len(key.String()) > objectstore.MaxKeyBytes {
			t.Fatalf("%s key invalid: %q", name, key.String())
		}
	}
}

func TestStrictEnvelopeRejectsCorruption(t *testing.T) {
	key := WriteGateKey()
	tests := [][]byte{
		[]byte(`{"schema":"write-gate-v1","schema":"write-gate-v1","revision":1,"logicalVersion":"x","payload":{}}`),
		[]byte(`{"schema":"write-gate-v1","revision":1,"logicalVersion":"x","payload":{},"unknown":true}`),
		append([]byte(`{"schema":"write-gate-v1","revision":1,"logicalVersion":"x","payload":{}}`), 0xff),
	}
	for _, input := range tests {
		var envelope Envelope
		var gate WriteGate
		if err := DecodeEnvelope(input, key, "write-gate-v1", &envelope, &gate); err == nil {
			t.Fatalf("DecodeEnvelope(%q) succeeded", input)
		}
	}

	valid, _ := EncodeEnvelope("write-gate-v1", key, 1, WriteGate{SchemaVersion: 1, Epoch: 1, Mode: GateOpen})
	var raw map[string]any
	_ = json.Unmarshal(valid, &raw)
	raw["logicalVersion"] = "tampered"
	tampered, _ := json.Marshal(raw)
	var envelope Envelope
	var gate WriteGate
	if err := DecodeEnvelope(tampered, key, "write-gate-v1", &envelope, &gate); err == nil {
		t.Fatal("tampered logical version accepted")
	}
}

func TestCanonicalFormatFailureAndValidationMatrix(t *testing.T) {
	key := WriteGateKey()
	if _, err := EncodeCanonical(failingCanonicalValue{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("canonical encoder failure = %v", err)
	}
	if _, err := EncodeCanonical(strings.Repeat("x", MaxCanonicalBytes)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized canonical value error = %v", err)
	}
	for _, test := range []struct {
		schema   string
		key      objectstore.Key
		revision uint64
	}{
		{key: key, revision: 1},
		{schema: "schema", revision: 1},
		{schema: "schema", key: key},
	} {
		if _, err := EncodeEnvelope(test.schema, test.key, test.revision, WriteGate{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid envelope input error = %v", err)
		}
	}
	if _, err := EncodeEnvelope("schema", key, 1, failingCanonicalValue{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("envelope payload error = %v", err)
	}

	valid, err := EncodeEnvelope("schema", key, 1, WriteGate{SchemaVersion: 1, Epoch: 1, Mode: GateOpen})
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	var gate WriteGate
	for _, decode := range []func() error{
		func() error { return DecodeEnvelope(valid, key, "schema", nil, &gate) },
		func() error { return DecodeEnvelope(valid, key, "schema", &envelope, nil) },
		func() error { return DecodeEnvelope(valid, objectstore.Key{}, "schema", &envelope, &gate) },
		func() error { return DecodeEnvelope(valid, key, "", &envelope, &gate) },
	} {
		if err := decode(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid decode destination error = %v", err)
		}
	}
	badFields := [][]byte{
		[]byte(`{"schema":"wrong","revision":1,"logicalVersion":"version","payload":{}}`),
		[]byte(`{"schema":"schema","revision":1,"logicalVersion":"version","payload":{"unknown":true}}`),
	}
	for _, body := range badFields {
		envelope = Envelope{}
		gate = WriteGate{}
		if err := DecodeEnvelope(body, key, "schema", &envelope, &gate); err == nil {
			t.Fatalf("invalid canonical envelope accepted: %s", body)
		}
	}
	envelope = Envelope{}
	if err := DecodeEnvelope(valid, key, "schema", &envelope, &decodeThenFailCanonicalValue{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("canonical payload re-encode error = %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(valid, &raw); err != nil {
		t.Fatal(err)
	}
	raw["payload"] = json.RawMessage("{ \"schemaVersion\":1,\"epoch\":1,\"mode\":\"open\"}")
	noncanonicalPayload, _ := json.Marshal(raw)
	if err := DecodeEnvelope(noncanonicalPayload, key, "schema", &envelope, &gate); err == nil {
		t.Fatal("non-canonical payload encoding was accepted")
	}
	if bytes.Equal(valid, append(valid, ' ')) {
		t.Fatal("invalid test fixture")
	}
	if err := DecodeEnvelope(append(valid, ' '), key, "schema", &envelope, &gate); err == nil {
		t.Fatal("non-canonical envelope encoding was accepted")
	}

	for _, invalid := range []WriteGate{{}, {SchemaVersion: 1, Epoch: 1, Mode: "unknown"}} {
		if err := ValidateGate(invalid); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid gate error = %v", err)
		}
	}
	if err := ValidateGate(WriteGate{SchemaVersion: 1, Epoch: 1, Mode: GateClosed}); err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"", "Upper", "under_score"} {
		if err := ValidateNamespace(namespace); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid namespace %q error = %v", namespace, err)
		}
	}
	if err := ValidateNamespace("valid-namespace"); err != nil {
		t.Fatal(err)
	}
	if !SortedUnique(nil) || !SortedUnique([]string{"a", "b"}) || SortedUnique([]string{"a", "a"}) || SortedUnique([]string{"b", "a"}) {
		t.Fatal("SortedUnique boundary mismatch")
	}
}

func TestCanonicalKeyNamespacePanicsFailClosed(t *testing.T) {
	for _, function := range []func(){
		func() { StateKey("INVALID", "key") },
		func() { StatePrefix("INVALID") },
		func() { StateVersionKey("INVALID", "key", "version") },
		func() { LeaseKey("INVALID", "lease") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid canonical namespace did not panic")
				}
			}()
			function()
		}()
	}
}
