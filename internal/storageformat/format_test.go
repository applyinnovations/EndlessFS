package storageformat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

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
