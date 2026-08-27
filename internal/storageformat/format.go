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
	FormatID                    = "endlessfs-portable-bucket-v1"
	CanonicalEncoder            = "canonical-json-v1"
	KeyFormatVersion            = 1
	WriterProtocolVersion       = 1
	MaxCanonicalBytes           = 1 << 20
	FeatureRecursiveBytes       = "recursive-byte-aggregates-v1"
	FeatureRecursiveFileCounts  = "recursive-file-count-aggregates-v1"
	FeatureProviderFingerprints = "provider-content-fingerprints-v1"
	FeatureDuplicateCatalog     = "duplicate-catalog-v1"
	FeatureDirectoryDigests     = "directory-content-digests-v1"
	FeatureMetadataCheckpoints  = "metadata-only-checkpoints-v1"
	FeaturePagedOperations      = "paged-operation-steps-v1"
	FeatureStateIndexes         = "persistent-state-indexes-v1"
	FeatureDirectoryIndexes     = "persistent-directory-indexes-v1"
	FeatureNamespaceSnapshots   = "persistent-namespace-snapshots-v1"
	FeatureResumableOperations  = "resumable-operation-preparation-v1"
	FeatureUserDirectoryCatalog = "user-addressable-duplicate-directories-v1"
	FeatureConsistencyDomains   = "consistency-domains-v1"
	FeatureOwnerNamespaceGraph  = "owner-namespace-graph-v1"
	FeatureDerivedProjections   = "rebuildable-derived-projections-v1"
	FeatureTransactionalState   = "transactional-state-domains-v1"
	FeatureStateConservation    = "state-conservation-v1"
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
		return domain.NewError(domain.ErrorInvalid, "non-canonical payload encoding for "+key.String())
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
	Action                 MutationAction            `json:"action"`
	TargetKey              string                    `json:"targetKey"`
	ExpectedLogicalVersion string                    `json:"expectedLogicalVersion,omitempty"`
	TargetBody             []byte                    `json:"targetBody,omitempty"`
	Prerequisites          []MutationObject          `json:"prerequisites,omitempty"`
	PrerequisiteRefs       []MutationObjectReference `json:"prerequisiteRefs,omitempty"`
	Copies                 []MutationCopy            `json:"copies,omitempty"`
	AbortUploads           []string                  `json:"abortUploads,omitempty"`
	CompleteUploads        []string                  `json:"completeUploads,omitempty"`
	RecoverOperationKey    string                    `json:"recoverOperationKey,omitempty"`
	RecoverUploadKey       string                    `json:"recoverUploadKey,omitempty"`
}

type MutationObject struct {
	Key  string `json:"key"`
	Body []byte `json:"body"`
}

type MutationCopy struct {
	SourceKey      string `json:"sourceKey"`
	DestinationKey string `json:"destinationKey"`
	Size           int64  `json:"size"`
	MD5            string `json:"md5,omitempty"`
	CRC32C         string `json:"crc32c,omitempty"`
	// SHA256 remains decodable only for historical epoch migrations. Current
	// writers use normalized provider-backed checksums and leave it empty.
	SHA256 string `json:"sha256,omitempty"`
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

type StateIndexRoot struct {
	SchemaVersion int    `json:"schemaVersion"`
	Namespace     string `json:"namespace"`
	NodeID        string `json:"nodeID,omitempty"`
	NodeDigest    string `json:"nodeDigest,omitempty"`
	EntryCount    uint64 `json:"entryCount"`
}

type StateIndexEntry struct {
	LogicalKey     string `json:"logicalKey"`
	LogicalVersion string `json:"logicalVersion"`
}

type StateIndexChild struct {
	NodeID     string `json:"nodeID"`
	NodeDigest string `json:"nodeDigest"`
	FirstKey   string `json:"firstKey"`
	LastKey    string `json:"lastKey"`
	EntryCount uint64 `json:"entryCount"`
}

type StateIndexNode struct {
	SchemaVersion int               `json:"schemaVersion"`
	Namespace     string            `json:"namespace"`
	NodeID        string            `json:"nodeID"`
	Leaf          bool              `json:"leaf"`
	Entries       []StateIndexEntry `json:"entries,omitempty"`
	Children      []StateIndexChild `json:"children,omitempty"`
}

type DirectoryRoot struct {
	SchemaVersion      int                  `json:"schemaVersion"`
	DirectoryID        string               `json:"directoryID"`
	ManifestID         string               `json:"manifestID"`
	RecursiveBytes     int64                `json:"recursiveBytes"`
	RecursiveFileCount int64                `json:"recursiveFileCount"`
	ContentAccumulator string               `json:"contentAccumulator,omitempty"`
	ContentDigest      string               `json:"contentDigest,omitempty"`
	Pending            *DirectoryTransition `json:"pending,omitempty"`
}

type DirectoryTransition struct {
	OperationID            string `json:"operationID"`
	Fence                  uint64 `json:"fence"`
	PreManifestID          string `json:"preManifestID,omitempty"`
	PostManifestID         string `json:"postManifestID"`
	PostRecursiveBytes     int64  `json:"postRecursiveBytes"`
	PostRecursiveFileCount int64  `json:"postRecursiveFileCount"`
	PostContentAccumulator string `json:"postContentAccumulator,omitempty"`
	PostContentDigest      string `json:"postContentDigest,omitempty"`
}

type DirectoryManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	DirectoryID   string `json:"directoryID"`
	ManifestID    string `json:"manifestID"`
	// PageIDs is retained only for migration decoding of schemas 001-003.
	PageIDs                []string                 `json:"pageIDs,omitempty"`
	IndexRootID            string                   `json:"indexRootID,omitempty"`
	IndexRootDigest        string                   `json:"indexRootDigest,omitempty"`
	SortIndexes            []DirectorySortIndexRoot `json:"sortIndexes,omitempty"`
	ContentIndexRootID     string                   `json:"contentIndexRootID,omitempty"`
	ContentIndexRootDigest string                   `json:"contentIndexRootDigest,omitempty"`
	ContentSketch          []string                 `json:"contentSketch,omitempty"`
	// ContentBase and ContentDeltas form a persistent lazy content view. A
	// namespace mutation can attach or detach an already-indexed subtree by
	// recording one bounded source reference instead of rewriting every
	// descendant occurrence. Exact readers merge the immutable base and deltas.
	ContentBase        *DirectoryContentBase   `json:"contentBase,omitempty"`
	ContentDeltas      []DirectoryContentDelta `json:"contentDeltas,omitempty"`
	EntryCount         int                     `json:"entryCount"`
	RecursiveBytes     int64                   `json:"recursiveBytes"`
	RecursiveFileCount int64                   `json:"recursiveFileCount"`
	ContentAccumulator string                  `json:"contentAccumulator,omitempty"`
	ContentDigest      string                  `json:"contentDigest,omitempty"`
	CreatedAt          time.Time               `json:"createdAt"`
}

type DirectorySortIndexRoot struct {
	Sort       domain.SortField `json:"sort"`
	NodeID     string           `json:"nodeID"`
	NodeDigest string           `json:"nodeDigest"`
}

type DirectorySortIndexEntry struct {
	SortKey string         `json:"sortKey"`
	Entry   DirectoryEntry `json:"entry"`
}

type DirectorySortIndexChild struct {
	NodeID     string `json:"nodeID"`
	NodeDigest string `json:"nodeDigest"`
	FirstKey   string `json:"firstKey"`
	LastKey    string `json:"lastKey"`
	EntryCount uint64 `json:"entryCount"`
}

type DirectorySortIndexNode struct {
	SchemaVersion int                       `json:"schemaVersion"`
	DirectoryID   string                    `json:"directoryID"`
	Sort          domain.SortField          `json:"sort"`
	NodeID        string                    `json:"nodeID"`
	Leaf          bool                      `json:"leaf"`
	Entries       []DirectorySortIndexEntry `json:"entries,omitempty"`
	Children      []DirectorySortIndexChild `json:"children,omitempty"`
}

// DirectoryContentIndexEntry is one file occurrence relative to the directory
// whose immutable manifest pins the index. Entries are ordered first by the
// provider-backed duplicate group and then by relative path.
type DirectoryContentIndexEntry struct {
	GroupID      string `json:"groupID"`
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
}

type DirectoryContentIndexChild struct {
	NodeID     string   `json:"nodeID"`
	NodeDigest string   `json:"nodeDigest"`
	FirstKey   string   `json:"firstKey"`
	LastKey    string   `json:"lastKey"`
	EntryCount uint64   `json:"entryCount"`
	Sketch     []string `json:"sketch"`
}

type DirectoryContentIndexNode struct {
	SchemaVersion int                          `json:"schemaVersion"`
	DirectoryID   string                       `json:"directoryID"`
	NodeID        string                       `json:"nodeID"`
	Leaf          bool                         `json:"leaf"`
	Entries       []DirectoryContentIndexEntry `json:"entries,omitempty"`
	Children      []DirectoryContentIndexChild `json:"children,omitempty"`
}

type DirectoryContentBase struct {
	Area        string `json:"area"`
	DirectoryID string `json:"directoryID"`
	ManifestID  string `json:"manifestID"`
}

type DirectoryContentDelta struct {
	Remove      bool                        `json:"remove"`
	Area        string                      `json:"area,omitempty"`
	DirectoryID string                      `json:"directoryID,omitempty"`
	ManifestID  string                      `json:"manifestID,omitempty"`
	Prefix      string                      `json:"prefix,omitempty"`
	Entry       *DirectoryContentIndexEntry `json:"entry,omitempty"`
}

type DirectoryIndexChild struct {
	NodeID             string `json:"nodeID"`
	NodeDigest         string `json:"nodeDigest"`
	FirstName          string `json:"firstName"`
	LastName           string `json:"lastName"`
	EntryCount         uint64 `json:"entryCount"`
	RecursiveBytes     int64  `json:"recursiveBytes"`
	RecursiveFileCount int64  `json:"recursiveFileCount"`
}

type DirectoryIndexNode struct {
	SchemaVersion int                   `json:"schemaVersion"`
	DirectoryID   string                `json:"directoryID"`
	NodeID        string                `json:"nodeID"`
	Leaf          bool                  `json:"leaf"`
	Entries       []DirectoryEntry      `json:"entries,omitempty"`
	Children      []DirectoryIndexChild `json:"children,omitempty"`
}

type DirectoryPage struct {
	SchemaVersion int              `json:"schemaVersion"`
	DirectoryID   string           `json:"directoryID"`
	PageID        string           `json:"pageID"`
	Entries       []DirectoryEntry `json:"entries"`
}

type DirectoryEntry struct {
	Name        string           `json:"name"`
	NameDigest  string           `json:"nameDigest"`
	Kind        domain.EntryKind `json:"kind"`
	DirectoryID string           `json:"directoryID,omitempty"`
	// ManifestID pins an immutable directory snapshot. Empty is the frozen
	// schema-005 directory root selected at the schema-006 boundary.
	ManifestID string `json:"manifestID,omitempty"`
	// StorageArea records where a moved snapshot's metadata remains stored. It
	// is independent from the entry's current live/trash namespace placement.
	StorageArea string `json:"storageArea,omitempty"`
	BlobID      string `json:"blobID,omitempty"`
	Size        int64  `json:"size"`
	FileCount   int64  `json:"fileCount,omitempty"`
	MediaType   string `json:"mediaType,omitempty"`
	MD5         string `json:"md5,omitempty"`
	CRC32C      string `json:"crc32c,omitempty"`
	// ContentDigest is present only for directories and identifies the exact
	// relative subtree content independently of the directory's own name.
	ContentDigest string `json:"contentDigest,omitempty"`
	// SHA256 remains decodable only for historical epoch migrations. Current
	// writers use normalized provider-backed checksums and leave it empty.
	SHA256         string    `json:"sha256,omitempty"`
	ModifiedAt     time.Time `json:"modifiedAt"`
	LogicalVersion string    `json:"logicalVersion"`
}

type UploadState string

const (
	UploadInitializing UploadState = "initializing"
	UploadActive       UploadState = "active"
	UploadCompleted    UploadState = "completed"
	UploadAborted      UploadState = "aborted"
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
	FileOperationPreparing FileOperationState = "preparing"
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
	SchemaVersion     int                       `json:"schemaVersion"`
	OperationID       string                    `json:"operationID"`
	UserID            string                    `json:"userID"`
	Kind              string                    `json:"kind"`
	State             FileOperationState        `json:"state"`
	Attempt           uint64                    `json:"attempt"`
	Fence             uint64                    `json:"fence"`
	ReplicaAttemptID  string                    `json:"replicaAttemptID"`
	ExpiresAt         time.Time                 `json:"expiresAt"`
	StartedAt         time.Time                 `json:"startedAt"`
	UpdatedAt         time.Time                 `json:"updatedAt"`
	ErrorKind         domain.ErrorKind          `json:"errorKind,omitempty"`
	Error             string                    `json:"error,omitempty"`
	IntentFingerprint string                    `json:"intentFingerprint,omitempty"`
	Roots             []FileOperationRoot       `json:"roots"`
	Prerequisites     []MutationObject          `json:"prerequisites,omitempty"`
	PrerequisiteRefs  []MutationObjectReference `json:"prerequisiteRefs,omitempty"`
	Copies            []MutationCopy            `json:"copies,omitempty"`
	StepPageCount     uint64                    `json:"stepPageCount,omitempty"`
	StepSetID         string                    `json:"stepSetID,omitempty"`
	StepDigest        string                    `json:"stepDigest,omitempty"`
	StepsStaged       bool                      `json:"stepsStaged,omitempty"`
	Preparation       *FileOperationPreparation `json:"preparation,omitempty"`
}

type FileOperationPreparation struct {
	SchemaVersion int                              `json:"schemaVersion"`
	RunSetID      string                           `json:"runSetID"`
	Phase         string                           `json:"phase"`
	Generation    uint64                           `json:"generation"`
	RunCount      uint64                           `json:"runCount"`
	GateEpoch     uint64                           `json:"gateEpoch,omitempty"`
	GateVersion   string                           `json:"gateVersion,omitempty"`
	Request       *FileOperationPreparationRequest `json:"request,omitempty"`
}

type FileOperationDirectoryPin struct {
	DirectoryID    string `json:"directoryID"`
	ManifestID     string `json:"manifestID"`
	LogicalVersion string `json:"logicalVersion"`
	PreExisted     bool   `json:"preExisted"`
}

// FileOperationPreparationRequest contains only portable canonical values. It
// is sufficient for a new replica to reconstruct the same deterministic
// bounded preparation runs without a process-local closure or provider-native
// version.
type FileOperationPreparationRequest struct {
	FromArea            string                     `json:"fromArea"`
	ToArea              string                     `json:"toArea"`
	Source              string                     `json:"source"`
	Destination         string                     `json:"destination,omitempty"`
	ResolvedDestination string                     `json:"resolvedDestination,omitempty"`
	Conflict            domain.ConflictMode        `json:"conflict,omitempty"`
	ExpectedSource      domain.Version             `json:"expectedSource,omitempty"`
	ExpectedTarget      domain.Version             `json:"expectedTarget,omitempty"`
	Fingerprint         string                     `json:"fingerprint"`
	Move                bool                       `json:"move,omitempty"`
	SourceEntry         DirectoryEntry             `json:"sourceEntry"`
	DestinationEntry    *DirectoryEntry            `json:"destinationEntry,omitempty"`
	SourceParent        FileOperationDirectoryPin  `json:"sourceParent"`
	DestinationParent   *FileOperationDirectoryPin `json:"destinationParent,omitempty"`
}

type FileOperationPreparationItemKind string

const (
	FileOperationPreparationRoot         FileOperationPreparationItemKind = "root"
	FileOperationPreparationPrerequisite FileOperationPreparationItemKind = "prerequisite"
	FileOperationPreparationCopy         FileOperationPreparationItemKind = "copy"
	FileOperationPreparationOccurrence   FileOperationPreparationItemKind = "occurrence"
	FileOperationPreparationSummary      FileOperationPreparationItemKind = "summary"
	FileOperationPreparationSimilarity   FileOperationPreparationItemKind = "similarity"
)

type FileOperationPreparationOccurrenceChange struct {
	Before bool                `json:"before"`
	Value  DuplicateOccurrence `json:"value"`
}

type FileOperationPreparationSummaryDelta struct {
	GroupID   string               `json:"groupID"`
	Kind      domain.DuplicateKind `json:"kind"`
	Shard     string               `json:"shard"`
	Size      int64                `json:"size"`
	FileCount int64                `json:"fileCount"`
	Delta     int64                `json:"delta"`
}

type FileOperationPreparationSimilarityChange struct {
	Before bool                       `json:"before"`
	Value  DuplicateSimilarityPosting `json:"value"`
}

type FileOperationPreparationItem struct {
	SortKey      string                                    `json:"sortKey"`
	Kind         FileOperationPreparationItemKind          `json:"kind"`
	Root         *FileOperationRoot                        `json:"root,omitempty"`
	Prerequisite *MutationObjectReference                  `json:"prerequisite,omitempty"`
	Copy         *MutationCopy                             `json:"copy,omitempty"`
	Occurrence   *FileOperationPreparationOccurrenceChange `json:"occurrence,omitempty"`
	Summary      *FileOperationPreparationSummaryDelta     `json:"summary,omitempty"`
	Similarity   *FileOperationPreparationSimilarityChange `json:"similarity,omitempty"`
}

type FileOperationPreparationPage struct {
	SchemaVersion  int                            `json:"schemaVersion"`
	UserID         string                         `json:"userID"`
	OperationID    string                         `json:"operationID"`
	RunSetID       string                         `json:"runSetID"`
	Generation     uint64                         `json:"generation"`
	Run            uint64                         `json:"run"`
	Page           uint64                         `json:"page"`
	PreviousDigest string                         `json:"previousDigest"`
	Final          bool                           `json:"final"`
	Items          []FileOperationPreparationItem `json:"items"`
}

type MutationObjectReference struct {
	Key        string `json:"key"`
	BodyDigest string `json:"bodyDigest"`
	StagingKey string `json:"stagingKey,omitempty"`
}

type FileOperationStepPage struct {
	SchemaVersion  int                       `json:"schemaVersion"`
	UserID         string                    `json:"userID"`
	OperationID    string                    `json:"operationID"`
	StepSetID      string                    `json:"stepSetID"`
	Index          uint64                    `json:"index"`
	PreviousDigest string                    `json:"previousDigest"`
	Roots          []FileOperationRoot       `json:"roots,omitempty"`
	Prerequisites  []MutationObjectReference `json:"prerequisites,omitempty"`
	Copies         []MutationCopy            `json:"copies,omitempty"`
}

type DuplicateOccurrence struct {
	GroupID   string               `json:"groupID"`
	Kind      domain.DuplicateKind `json:"kind"`
	Area      string               `json:"area"`
	Path      string               `json:"path"`
	Size      int64                `json:"size"`
	FileCount int64                `json:"fileCount"`
	Version   string               `json:"version"`
}

type DuplicateOccurrenceTransition struct {
	OperationID string               `json:"operationID"`
	Fence       uint64               `json:"fence"`
	Pre         *DuplicateOccurrence `json:"pre,omitempty"`
	Post        *DuplicateOccurrence `json:"post,omitempty"`
}

// DuplicateOccurrenceRoot is a small mutable visibility root. A nil Current
// is a tombstone retained only until the bounded maintenance collector runs.
type DuplicateOccurrenceRoot struct {
	SchemaVersion int                            `json:"schemaVersion"`
	UserID        string                         `json:"userID"`
	Current       *DuplicateOccurrence           `json:"current,omitempty"`
	Pending       *DuplicateOccurrenceTransition `json:"pending,omitempty"`
}

type DuplicateSummary struct {
	GroupID         string               `json:"groupID"`
	Kind            domain.DuplicateKind `json:"kind"`
	Shard           string               `json:"shard"`
	OccurrenceCount int64                `json:"occurrenceCount"`
	Size            int64                `json:"size"`
	FileCount       int64                `json:"fileCount"`
}

type DuplicateSummaryTransition struct {
	OperationID string            `json:"operationID"`
	Fence       uint64            `json:"fence"`
	Pre         *DuplicateSummary `json:"pre,omitempty"`
	Post        *DuplicateSummary `json:"post,omitempty"`
}

type DuplicateSummaryRoot struct {
	SchemaVersion int                         `json:"schemaVersion"`
	UserID        string                      `json:"userID"`
	Current       *DuplicateSummary           `json:"current,omitempty"`
	Pending       *DuplicateSummaryTransition `json:"pending,omitempty"`
}

type DuplicateSimilarityPosting struct {
	Position    int    `json:"position"`
	SketchValue string `json:"sketchValue"`
	Area        string `json:"area"`
	DirectoryID string `json:"directoryID"`
	Path        string `json:"path"`
}

type DuplicateSimilarityPostingTransition struct {
	OperationID string                      `json:"operationID"`
	Fence       uint64                      `json:"fence"`
	Pre         *DuplicateSimilarityPosting `json:"pre,omitempty"`
	Post        *DuplicateSimilarityPosting `json:"post,omitempty"`
}

type DuplicateSimilarityPostingRoot struct {
	SchemaVersion int                                   `json:"schemaVersion"`
	UserID        string                                `json:"userID"`
	Current       *DuplicateSimilarityPosting           `json:"current,omitempty"`
	Pending       *DuplicateSimilarityPostingTransition `json:"pending,omitempty"`
}

type DuplicateIgnore struct {
	SchemaVersion int    `json:"schemaVersion"`
	UserID        string `json:"userID"`
	GroupID       string `json:"groupID"`
	Ignored       bool   `json:"ignored"`
	Revision      uint64 `json:"revision"`
}

type DuplicateDirectoryIgnore struct {
	SchemaVersion    int    `json:"schemaVersion"`
	UserID           string `json:"userID"`
	PairID           string `json:"pairID"`
	LeftArea         string `json:"leftArea"`
	LeftDirectoryID  string `json:"leftDirectoryID"`
	RightArea        string `json:"rightArea"`
	RightDirectoryID string `json:"rightDirectoryID"`
	Ignored          bool   `json:"ignored"`
	Revision         uint64 `json:"revision"`
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
	SHA256 string `json:"sha256,omitempty"`
	MD5    string `json:"md5,omitempty"`
	CRC32C string `json:"crc32c,omitempty"`
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
	Objects               []CheckpointObject `json:"objects,omitempty"`
	InventoryPageCount    uint64             `json:"inventoryPageCount,omitempty"`
	StateObjectCount      uint64             `json:"stateObjectCount,omitempty"`
	FileObjectCount       uint64             `json:"fileObjectCount,omitempty"`
	InventoryDigest       string             `json:"inventoryDigest"`
}

type CheckpointInventoryEntry struct {
	FileData bool             `json:"fileData"`
	Object   CheckpointObject `json:"object"`
}

type CheckpointInventoryPage struct {
	SchemaVersion  int                        `json:"schemaVersion"`
	CheckpointID   string                     `json:"checkpointID"`
	GateEpoch      uint64                     `json:"gateEpoch"`
	Index          uint64                     `json:"index"`
	PreviousDigest string                     `json:"previousDigest"`
	Entries        []CheckpointInventoryEntry `json:"entries"`
}

type GarbageCollectionSession struct {
	SchemaVersion int       `json:"schemaVersion"`
	CheckpointID  string    `json:"checkpointID"`
	GateEpoch     uint64    `json:"gateEpoch"`
	GateVersion   string    `json:"gateVersion"`
	Phase         string    `json:"phase"`
	SweepIndex    int       `json:"sweepIndex"`
	After         string    `json:"after,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type GarbageCollectionMark struct {
	SchemaVersion int    `json:"schemaVersion"`
	CheckpointID  string `json:"checkpointID"`
	GateEpoch     uint64 `json:"gateEpoch"`
	GateVersion   string `json:"gateVersion"`
	Role          string `json:"role"`
	TargetKey     string `json:"targetKey"`
}

type MigrationDirectoryMark struct {
	SchemaVersion      int    `json:"schemaVersion"`
	CheckpointID       string `json:"checkpointID"`
	Phase              string `json:"phase"`
	UserID             string `json:"userID"`
	Area               string `json:"area"`
	DirectoryID        string `json:"directoryID"`
	ParentDirectoryID  string `json:"parentDirectoryID,omitempty"`
	ParentEntryName    string `json:"parentEntryName,omitempty"`
	ManifestID         string `json:"manifestID"`
	RootLogicalVersion string `json:"rootLogicalVersion"`
	ManifestVersion    string `json:"manifestLogicalVersion"`
	RecursiveBytes     int64  `json:"recursiveBytes"`
	RecursiveFileCount int64  `json:"recursiveFileCount"`
	DirectoryCount     int64  `json:"directoryCount"`
	ContentAccumulator string `json:"contentAccumulator,omitempty"`
	ContentDigest      string `json:"contentDigest,omitempty"`
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
