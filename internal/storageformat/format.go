// Package storageformat owns the canonical EndlessFS v1 key and body format.
package storageformat

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	FormatID                   = "endlessfs-portable-bucket-v1"
	CanonicalEncoder           = "canonical-json-v1"
	KeyFormatVersion           = 1
	WriterProtocolVersion      = 1
	MaxCanonicalBytes          = 1 << 20
	FeatureRecursiveBytes      = "recursive-byte-aggregates-v1"
	FeatureRecursiveFileCounts = "recursive-file-count-aggregates-v1"
)

type Envelope struct {
	Schema         string          `json:"schema"`
	Revision       uint64          `json:"revision"`
	LogicalVersion string          `json:"logicalVersion"`
	Payload        json.RawMessage `json:"payload"`
}

func EncodeCanonical(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "encode canonical record", err)
	}
	data := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	if len(data) == 0 || len(data) > MaxCanonicalBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "canonical record exceeds size limit")
	}
	return append([]byte(nil), data...), nil
}

func EncodeEnvelope(schema string, key objectstore.Key, revision uint64, payload any) ([]byte, error) {
	if schema == "" || !key.Valid() || revision == 0 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid canonical envelope")
	}
	payloadBytes, err := EncodeCanonical(payload)
	if err != nil {
		return nil, err
	}
	version := logicalVersion(key, revision, payloadBytes)
	envelope := struct {
		Schema         string          `json:"schema"`
		Revision       uint64          `json:"revision"`
		LogicalVersion string          `json:"logicalVersion"`
		Payload        json.RawMessage `json:"payload"`
	}{schema, revision, version, payloadBytes}
	return EncodeCanonical(envelope)
}

func DecodeEnvelope(data []byte, key objectstore.Key, schema string, envelope *Envelope, payload any) error {
	if envelope == nil || payload == nil || !key.Valid() || schema == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid canonical envelope destination")
	}
	if err := state.DecodeJSONWithLimit(data, envelope, MaxCanonicalBytes); err != nil {
		return err
	}
	if envelope.Schema != schema || envelope.Revision == 0 || envelope.LogicalVersion == "" || len(envelope.Payload) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid canonical envelope fields")
	}
	if err := state.DecodeJSONWithLimit(envelope.Payload, payload, MaxCanonicalBytes); err != nil {
		return err
	}
	payloadBytes, err := EncodeCanonical(payload)
	if err != nil {
		return err
	}
	if !bytes.Equal(payloadBytes, envelope.Payload) {
		return domain.NewError(domain.ErrorInvalid, "non-canonical payload encoding")
	}
	want := logicalVersion(key, envelope.Revision, payloadBytes)
	if envelope.LogicalVersion != want {
		return domain.NewError(domain.ErrorInvalid, "canonical logical version mismatch")
	}
	canonical, err := EncodeEnvelope(schema, key, envelope.Revision, payload)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, data) {
		return domain.NewError(domain.ErrorInvalid, "non-canonical envelope encoding")
	}
	return nil
}

func logicalVersion(key objectstore.Key, revision uint64, payload []byte) string {
	payloadDigest := sha256.Sum256(payload)
	hash := sha256.New()
	_, _ = hash.Write([]byte("endlessfs-logical-version-v1"))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(key.String()))
	_, _ = hash.Write([]byte{0})
	var encodedRevision [8]byte
	binary.BigEndian.PutUint64(encodedRevision[:], revision)
	_, _ = hash.Write(encodedRevision[:])
	_, _ = hash.Write(payloadDigest[:])
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

type Superblock struct {
	SchemaVersion         int       `json:"schemaVersion"`
	FormatID              string    `json:"formatID"`
	BucketID              string    `json:"bucketID"`
	CanonicalEncoder      string    `json:"canonicalEncoder"`
	KeyFormatVersion      int       `json:"keyFormatVersion"`
	WriterProtocolVersion int       `json:"writerProtocolVersion"`
	CreatedAt             time.Time `json:"createdAt"`
	RequiredFeatures      []string  `json:"requiredFeatures"`
}

type WriterSet struct {
	SchemaVersion         int      `json:"schemaVersion"`
	WriterSetID           string   `json:"writerSetID"`
	WriterProtocolVersion int      `json:"writerProtocolVersion"`
	RequiredFeatures      []string `json:"requiredFeatures"`
	ConfigurationDigest   string   `json:"configurationDigest"`
	KeyringIdentifiers    []string `json:"keyringIdentifiers"`
	MinimumReaderProtocol int      `json:"minimumReaderProtocol"`
	MaximumReaderProtocol int      `json:"maximumReaderProtocol"`
	MinimumWriterProtocol int      `json:"minimumWriterProtocol"`
	MaximumWriterProtocol int      `json:"maximumWriterProtocol"`
}

type GateMode string

const (
	GateOpen    GateMode = "open"
	GateClosing GateMode = "closing"
	GateClosed  GateMode = "closed"
)

type WriteGate struct {
	SchemaVersion  int      `json:"schemaVersion"`
	Epoch          uint64   `json:"epoch"`
	Mode           GateMode `json:"mode"`
	CheckpointID   string   `json:"checkpointID,omitempty"`
	WriterFeatures []string `json:"writerFeatures,omitempty"`
}

type AdmissionState string

const (
	AdmissionCandidate AdmissionState = "candidate"
	AdmissionAdmitted  AdmissionState = "admitted"
	AdmissionCancelled AdmissionState = "cancelled"
)

type Admission struct {
	SchemaVersion    int             `json:"schemaVersion"`
	Epoch            uint64          `json:"epoch"`
	OperationID      string          `json:"operationID"`
	WriterSetID      string          `json:"writerSetID"`
	ReplicaAttemptID string          `json:"replicaAttemptID"`
	ObservedGate     string          `json:"observedGateLogicalVersion"`
	State            AdmissionState  `json:"state"`
	Attempt          uint64          `json:"attempt"`
	Fence            uint64          `json:"fence"`
	CreatedAt        time.Time       `json:"createdAt"`
	ExpiresAt        time.Time       `json:"expiresAt"`
	IntentDigest     string          `json:"intentDigest"`
	Mutation         *MutationIntent `json:"mutation,omitempty"`
}

type MutationAction string

const (
	MutationCreate MutationAction = "create"
	MutationCAS    MutationAction = "compare-and-swap"
	MutationDelete MutationAction = "delete"
)

type MutationIntent struct {
	Action                 MutationAction   `json:"action"`
	TargetKey              string           `json:"targetKey"`
	ExpectedLogicalVersion string           `json:"expectedLogicalVersion,omitempty"`
	TargetBody             []byte           `json:"targetBody,omitempty"`
	Prerequisites          []MutationObject `json:"prerequisites,omitempty"`
	Copies                 []MutationCopy   `json:"copies,omitempty"`
	AbortUploads           []string         `json:"abortUploads,omitempty"`
	RecoverOperationKey    string           `json:"recoverOperationKey,omitempty"`
	RecoverUploadKey       string           `json:"recoverUploadKey,omitempty"`
}

type MutationObject struct {
	Key  string `json:"key"`
	Body []byte `json:"body"`
}

type MutationCopy struct {
	SourceKey      string `json:"sourceKey"`
	DestinationKey string `json:"destinationKey"`
	Size           int64  `json:"size"`
	SHA256         string `json:"sha256,omitempty"`
}

type StateRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	LogicalKey    string `json:"logicalKey"`
	Data          []byte `json:"data"`
}

type StateVersionRecord struct {
	SchemaVersion  int    `json:"schemaVersion"`
	LogicalKey     string `json:"logicalKey"`
	LogicalVersion string `json:"logicalVersion"`
	Data           []byte `json:"data"`
}

type DirectoryRoot struct {
	SchemaVersion      int                  `json:"schemaVersion"`
	DirectoryID        string               `json:"directoryID"`
	ManifestID         string               `json:"manifestID"`
	RecursiveBytes     int64                `json:"recursiveBytes"`
	RecursiveFileCount int64                `json:"recursiveFileCount"`
	Pending            *DirectoryTransition `json:"pending,omitempty"`
}

type DirectoryTransition struct {
	OperationID            string `json:"operationID"`
	Fence                  uint64 `json:"fence"`
	PreManifestID          string `json:"preManifestID,omitempty"`
	PostManifestID         string `json:"postManifestID"`
	PostRecursiveBytes     int64  `json:"postRecursiveBytes"`
	PostRecursiveFileCount int64  `json:"postRecursiveFileCount"`
}

type DirectoryManifest struct {
	SchemaVersion      int       `json:"schemaVersion"`
	DirectoryID        string    `json:"directoryID"`
	ManifestID         string    `json:"manifestID"`
	PageIDs            []string  `json:"pageIDs"`
	EntryCount         int       `json:"entryCount"`
	RecursiveBytes     int64     `json:"recursiveBytes"`
	RecursiveFileCount int64     `json:"recursiveFileCount"`
	CreatedAt          time.Time `json:"createdAt"`
}

type DirectoryPage struct {
	SchemaVersion int              `json:"schemaVersion"`
	DirectoryID   string           `json:"directoryID"`
	PageID        string           `json:"pageID"`
	Entries       []DirectoryEntry `json:"entries"`
}

type DirectoryEntry struct {
	Name           string           `json:"name"`
	NameDigest     string           `json:"nameDigest"`
	Kind           domain.EntryKind `json:"kind"`
	DirectoryID    string           `json:"directoryID,omitempty"`
	BlobID         string           `json:"blobID,omitempty"`
	Size           int64            `json:"size"`
	FileCount      int64            `json:"fileCount,omitempty"`
	MediaType      string           `json:"mediaType,omitempty"`
	SHA256         string           `json:"sha256,omitempty"`
	ModifiedAt     time.Time        `json:"modifiedAt"`
	LogicalVersion string           `json:"logicalVersion"`
}

type UploadState string

const (
	UploadActive    UploadState = "active"
	UploadCompleted UploadState = "completed"
	UploadAborted   UploadState = "aborted"
)

type UploadRecord struct {
	SchemaVersion         int                 `json:"schemaVersion"`
	UploadID              string              `json:"uploadID"`
	CompletionOperationID string              `json:"completionOperationID"`
	UserID                string              `json:"userID"`
	Area                  string              `json:"area"`
	RequestedPath         string              `json:"requestedPath"`
	ResolvedPath          string              `json:"resolvedPath"`
	StagingKey            string              `json:"stagingKey"`
	BackendKind           string              `json:"backendKind,omitempty"`
	LeaseKey              string              `json:"leaseKey,omitempty"`
	Size                  int64               `json:"size"`
	MediaType             string              `json:"mediaType"`
	Conflict              domain.ConflictMode `json:"conflict"`
	ExpectedVersion       domain.Version      `json:"expectedVersion,omitempty"`
	TargetExisted         bool                `json:"targetExisted"`
	Resumable             bool                `json:"resumable"`
	State                 UploadState         `json:"state"`
	CreatedAt             time.Time           `json:"createdAt"`
	ExpiresAt             time.Time           `json:"expiresAt"`
}

type TransferLease struct {
	SchemaVersion int       `json:"schemaVersion"`
	BackendKind   string    `json:"backendKind"`
	UploadID      string    `json:"uploadID"`
	Ciphertext    []byte    `json:"ciphertext"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type FileOperationState string

const (
	FileOperationRunning   FileOperationState = "running"
	FileOperationCommitted FileOperationState = "committed"
	FileOperationSucceeded FileOperationState = "succeeded"
	FileOperationFailed    FileOperationState = "failed"
)

type FileOperationRoot struct {
	Key                    string `json:"key"`
	ExpectedLogicalVersion string `json:"expectedLogicalVersion,omitempty"`
	PreExisted             bool   `json:"preExisted"`
	PendingBody            []byte `json:"pendingBody"`
	FinalBody              []byte `json:"finalBody"`
	RollbackBody           []byte `json:"rollbackBody,omitempty"`
}

type FileOperation struct {
	SchemaVersion    int                 `json:"schemaVersion"`
	OperationID      string              `json:"operationID"`
	UserID           string              `json:"userID"`
	Kind             string              `json:"kind"`
	State            FileOperationState  `json:"state"`
	Attempt          uint64              `json:"attempt"`
	Fence            uint64              `json:"fence"`
	ReplicaAttemptID string              `json:"replicaAttemptID"`
	ExpiresAt        time.Time           `json:"expiresAt"`
	StartedAt        time.Time           `json:"startedAt"`
	UpdatedAt        time.Time           `json:"updatedAt"`
	ErrorKind        domain.ErrorKind    `json:"errorKind,omitempty"`
	Error            string              `json:"error,omitempty"`
	Roots            []FileOperationRoot `json:"roots"`
	Prerequisites    []MutationObject    `json:"prerequisites,omitempty"`
	Copies           []MutationCopy      `json:"copies,omitempty"`
}

type IdempotencyRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	UserID        string `json:"userID"`
	Kind          string `json:"kind"`
	KeyDigest     string `json:"keyDigest"`
	Fingerprint   string `json:"fingerprint"`
	OperationID   string `json:"operationID"`
}

type CheckpointObject struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Checkpoint struct {
	SchemaVersion         int                `json:"schemaVersion"`
	CheckpointID          string             `json:"checkpointID"`
	BucketID              string             `json:"bucketID"`
	WriterSetID           string             `json:"writerSetID"`
	GateEpoch             uint64             `json:"gateEpoch"`
	KeyFormatVersion      int                `json:"keyFormatVersion"`
	WriterProtocolVersion int                `json:"writerProtocolVersion"`
	CreatedAt             time.Time          `json:"createdAt"`
	Objects               []CheckpointObject `json:"objects"`
	InventoryDigest       string             `json:"inventoryDigest"`
}

// CheckpointWork records one completed provider-independent inventory digest.
// It is durable only while a closed-gate checkpoint is being constructed and
// is excluded from the checkpoint's authoritative inventory.
type CheckpointWork struct {
	SchemaVersion int              `json:"schemaVersion"`
	CheckpointID  string           `json:"checkpointID"`
	GateEpoch     uint64           `json:"gateEpoch"`
	FileData      bool             `json:"fileData"`
	Object        CheckpointObject `json:"object"`
	CRC32C        string           `json:"crc32c"`
	Proof         string           `json:"proof"`
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func ValidateGate(gate WriteGate) error {
	if gate.SchemaVersion != 1 || gate.Epoch == 0 || (gate.Mode != GateOpen && gate.Mode != GateClosing && gate.Mode != GateClosed) {
		return domain.NewError(domain.ErrorInvalid, "invalid write gate")
	}
	if len(gate.WriterFeatures) > 0 && !SortedUnique(gate.WriterFeatures) {
		return domain.NewError(domain.ErrorInvalid, "invalid write-gate feature binding")
	}
	return nil
}

func ValidateNamespace(value string) error {
	if value == "" {
		return domain.NewError(domain.ErrorInvalid, "empty canonical namespace")
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || character == '-' {
			continue
		}
		return domain.NewError(domain.ErrorInvalid, fmt.Sprintf("invalid canonical namespace %q", value))
	}
	return nil
}

func SortedUnique(values []string) bool {
	for index := 1; index < len(values); index++ {
		if strings.Compare(values[index-1], values[index]) >= 0 {
			return false
		}
	}
	return true
}
