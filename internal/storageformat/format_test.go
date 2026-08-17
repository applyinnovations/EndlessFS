package storageformat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

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
