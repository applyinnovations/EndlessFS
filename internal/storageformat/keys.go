package storageformat

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const root = "endlessfs/v1/"

const RootDirectoryID = "root"

var base32Lower = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func encodedPart(value string) string {
	return base32Lower.EncodeToString([]byte(value))
}

func digestPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base32Lower.EncodeToString(sum[:])
}

func NameDigest(value string) string { return digestPart(value) }

func fixedKey(value string) objectstore.Key { return objectstore.MustKey(root + value) }

func SuperblockKey() objectstore.Key { return fixedKey("superblock.json") }
func WriterSetKey() objectstore.Key  { return fixedKey("control/writer-set.json") }
func WriteGateKey() objectstore.Key  { return fixedKey("control/write-gate.json") }

func DomainCatalogHeadKey() objectstore.Key { return fixedKey("domains/catalog/head.json") }

func DomainCatalogPageKey(pageID string) objectstore.Key {
	validateDomainKeyPart(pageID)
	return fixedKey("domains/catalog/pages/" + digestPart(pageID) + ".json")
}

func DomainHeadKey(kind ConsistencyDomainKind, domainID string) objectstore.Key {
	validateDomainKeyPart(domainID)
	return fixedKey("domains/" + domainKeyKind(kind) + "/" + digestPart(domainID) + "/head.json")
}

func DomainPageKey(kind ConsistencyDomainKind, domainID, pageID string) objectstore.Key {
	validateDomainKeyPart(domainID)
	validateDomainKeyPart(pageID)
	return fixedKey("domains/" + domainKeyKind(kind) + "/" + digestPart(domainID) + "/pages/" + digestPart(pageID) + ".json")
}

func DomainSnapshotKey(kind ConsistencyDomainKind, domainID, digest string) objectstore.Key {
	validateDomainKeyPart(domainID)
	validateDomainKeyPart(digest)
	return fixedKey("domains/" + domainKeyKind(kind) + "/" + digestPart(domainID) + "/snapshots/" + digestPart(digest) + ".json")
}

func StateQuerySnapshotKey(digest string) objectstore.Key {
	validateDomainKeyPart(digest)
	return fixedKey("domains/state-query-snapshots/" + digestPart(digest) + ".json")
}

func StateQuerySnapshotPrefix() string { return root + "domains/state-query-snapshots/" }

func DomainPrefix() string { return root + "domains/" }

func Schema008MigrationStageKey(domainIdentity, sourceIdentity string) objectstore.Key {
	validateDomainKeyPart(domainIdentity)
	validateDomainKeyPart(sourceIdentity)
	return fixedKey("migrations/schema-007-to-008/staged/" + digestPart(domainIdentity) + "/" + digestPart(sourceIdentity) + ".json")
}

func Schema008MigrationStagePrefix() string {
	return root + "migrations/schema-007-to-008/staged/"
}

func Schema008MigrationStageDomainPrefix(domainIdentity string) string {
	validateDomainKeyPart(domainIdentity)
	return Schema008MigrationStagePrefix() + digestPart(domainIdentity) + "/"
}

func Schema008MigrationSourceMarkerKey(sourceIdentity string) objectstore.Key {
	validateDomainKeyPart(sourceIdentity)
	return fixedKey("migrations/schema-007-to-008/sources/" + digestPart(sourceIdentity) + ".json")
}

func Schema008MigrationSourceMarkerPrefix() string {
	return root + "migrations/schema-007-to-008/sources/"
}

func Schema008MigrationSubtreeKey(sourceIdentity string) objectstore.Key {
	validateDomainKeyPart(sourceIdentity)
	return fixedKey("migrations/schema-007-to-008/subtrees/" + digestPart(sourceIdentity) + ".json")
}

func Schema008MigrationSubtreePrefix() string {
	return root + "migrations/schema-007-to-008/subtrees/"
}

func ProjectionHeadKey(ownerID string, kind ProjectionKind) objectstore.Key {
	validateDomainKeyPart(ownerID)
	return fixedKey("projections/" + digestPart(ownerID) + "/" + projectionKeyKind(kind) + "/head.json")
}

func ScopedProjectionHeadKey(ownerID string, kind ProjectionKind, projectionID string) objectstore.Key {
	validateDomainKeyPart(ownerID)
	validateDomainKeyPart(projectionID)
	return fixedKey("projections/" + digestPart(ownerID) + "/" + projectionKeyKind(kind) + "/heads/" + digestPart(projectionID) + ".json")
}

func ProjectionPageKey(ownerID string, kind ProjectionKind, pageID string) objectstore.Key {
	validateDomainKeyPart(ownerID)
	validateDomainKeyPart(pageID)
	return fixedKey("projections/" + digestPart(ownerID) + "/" + projectionKeyKind(kind) + "/pages/" + digestPart(pageID) + ".json")
}

func ProjectionPrefix() string { return root + "projections/" }

func StateKey(namespace, logicalKey string) objectstore.Key {
	if err := ValidateNamespace(namespace); err != nil {
		panic(err)
	}
	return fixedKey("state/" + namespace + "/" + digestPart(logicalKey) + ".json")
}

func StatePrefix(namespace string) string {
	if err := ValidateNamespace(namespace); err != nil {
		panic(err)
	}
	return root + "state/" + namespace + "/"
}

func StateRecordsPrefix() string  { return root + "state/" }
func StateVersionsPrefix() string { return root + "state-versions/" }

func StateIndexRootKey(namespace string) objectstore.Key {
	if err := ValidateNamespace(namespace); err != nil {
		panic(err)
	}
	return fixedKey("state-indexes/" + namespace + "/root.json")
}

func StateIndexNodeKey(namespace, nodeID string) objectstore.Key {
	if err := ValidateNamespace(namespace); err != nil {
		panic(err)
	}
	return fixedKey("state-indexes/" + namespace + "/nodes/" + encodedPart(nodeID) + ".json")
}

func StateIndexRootPrefix() string { return root + "state-indexes/" }

func StateVersionKey(namespace, logicalKey, logicalVersion string) objectstore.Key {
	if err := ValidateNamespace(namespace); err != nil {
		panic(err)
	}
	return fixedKey("state-versions/" + namespace + "/" + digestPart(logicalKey) + "/" + digestPart(logicalVersion) + ".json")
}

func StateVersionLogicalPrefix(namespace, logicalKey string) string {
	if err := ValidateNamespace(namespace); err != nil {
		panic(err)
	}
	return root + "state-versions/" + namespace + "/" + digestPart(logicalKey) + "/"
}

func AdmissionKey(epoch uint64, operationID string) objectstore.Key {
	return fixedKey("admissions/" + strconv.FormatUint(epoch, 10) + "/" + digestPart(operationID) + ".json")
}

func AdmissionPrefix(epoch uint64) string {
	return root + "admissions/" + strconv.FormatUint(epoch, 10) + "/"
}

func StagingKey(userID, operationID, artifactID string) objectstore.Key {
	return fixedKey("staging/" + encodedPart(userID) + "/" + encodedPart(operationID) + "/" + encodedPart(artifactID))
}

func BlobKey(userID, blobID string) objectstore.Key {
	return fixedKey("fs/" + encodedPart(userID) + "/blobs/" + encodedPart(blobID))
}

func DirectoryRootKey(userID, area, directoryID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"fs", encodedPart(userID), area, "dirs", encodedPart(directoryID), "directory.json"}, "/"))
}

func DirectoryManifestKey(userID, area, directoryID, manifestID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"fs", encodedPart(userID), area, "dirs", encodedPart(directoryID), "manifests", encodedPart(manifestID) + ".json"}, "/"))
}

func DirectoryPageKey(userID, area, directoryID, pageID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"fs", encodedPart(userID), area, "dirs", encodedPart(directoryID), "pages", encodedPart(pageID) + ".json"}, "/"))
}

func DirectoryIndexNodeKey(userID, area, directoryID, nodeID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"fs", encodedPart(userID), area, "dirs", encodedPart(directoryID), "index", encodedPart(nodeID) + ".json"}, "/"))
}

func DirectorySortIndexNodeKey(userID, area, directoryID string, sort domain.SortField, nodeID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"fs", encodedPart(userID), area, "dirs", encodedPart(directoryID), "sort-index", string(sort), encodedPart(nodeID) + ".json"}, "/"))
}

func DirectoryContentIndexNodeKey(userID, area, directoryID, nodeID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"fs", encodedPart(userID), area, "dirs", encodedPart(directoryID), "content-index", encodedPart(nodeID) + ".json"}, "/"))
}

func FilesystemPrefix() string { return root + "fs/" }

func DuplicateOccurrenceKey(userID, kind, groupID, area, path string) objectstore.Key {
	return fixedKey(strings.Join([]string{"duplicates", encodedPart(userID), kind, "occurrences", encodedPart(groupID), digestPart(area+"\x00"+path) + ".json"}, "/"))
}

func DuplicateOccurrenceGroupPrefix(userID, kind, groupID string) string {
	return root + strings.Join([]string{"duplicates", encodedPart(userID), kind, "occurrences", encodedPart(groupID), ""}, "/")
}

func DuplicateOccurrenceOwnerPrefix(userID string) string {
	return root + strings.Join([]string{"duplicates", encodedPart(userID), ""}, "/")
}

func DuplicateSummaryKey(userID, kind, groupID, shard string) objectstore.Key {
	return fixedKey(strings.Join([]string{"duplicates", encodedPart(userID), kind, "summaries", encodedPart(groupID), shard + ".json"}, "/"))
}

func DuplicateSummaryPrefix(userID, kind string) string {
	return root + strings.Join([]string{"duplicates", encodedPart(userID), kind, "summaries", ""}, "/")
}

func DuplicateIgnoreKey(userID, groupID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"duplicates", encodedPart(userID), "ignores", encodedPart(groupID) + ".json"}, "/"))
}

func DuplicateDirectoryIgnoreKey(userID, pairID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"duplicates", encodedPart(userID), "directory-ignores", encodedPart(pairID) + ".json"}, "/"))
}

func DuplicateSimilarityPostingKey(userID string, position int, sketchValue, area, directoryID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"duplicates", encodedPart(userID), "similarity", fmt.Sprintf("%02x", position), encodedPart(sketchValue), area, encodedPart(directoryID) + ".json"}, "/"))
}

func DuplicateSimilarityPostingPrefix(userID string, position int, sketchValue string) string {
	return root + strings.Join([]string{"duplicates", encodedPart(userID), "similarity", fmt.Sprintf("%02x", position), encodedPart(sketchValue), ""}, "/")
}

func DuplicateRecordsPrefix() string { return root + "duplicates/" }

// ParseDirectoryRootKey validates and decodes a canonical directory-root key.
// The boolean is false for canonical filesystem objects of another kind.
func ParseDirectoryRootKey(key objectstore.Key) (userID, area, directoryID string, matched bool, err error) {
	segments := strings.Split(key.String(), "/")
	if len(segments) != 8 || segments[0] != "endlessfs" || segments[1] != "v1" || segments[2] != "fs" || segments[5] != "dirs" || segments[7] != "directory.json" {
		return "", "", "", false, nil
	}
	decode := func(value string) (string, error) {
		decoded, decodeErr := base32Lower.DecodeString(value)
		if decodeErr != nil || encodedPart(string(decoded)) != value {
			return "", domain.NewError(domain.ErrorInvalid, "invalid encoded directory-root key component")
		}
		return string(decoded), nil
	}
	userID, err = decode(segments[3])
	if err != nil {
		return "", "", "", true, err
	}
	if _, parseErr := domain.ParseUserID(userID); parseErr != nil {
		return "", "", "", true, parseErr
	}
	area = segments[4]
	if area != "live" && area != "trash" {
		return "", "", "", true, domain.NewError(domain.ErrorInvalid, "invalid directory-root area")
	}
	directoryID, err = decode(segments[6])
	if err != nil {
		return "", "", "", true, err
	}
	if directoryID != RootDirectoryID {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(directoryID)
		if decodeErr != nil || len(decoded) < 16 || base64.RawURLEncoding.EncodeToString(decoded) != directoryID {
			return "", "", "", true, domain.NewError(domain.ErrorInvalid, "invalid directory ID in root key")
		}
	}
	if DirectoryRootKey(userID, area, directoryID) != key {
		return "", "", "", true, domain.NewError(domain.ErrorInvalid, "non-canonical directory-root key")
	}
	return userID, area, directoryID, true, nil
}

func OperationKey(userID, operationID string) objectstore.Key {
	return fixedKey("operations/" + encodedPart(userID) + "/" + encodedPart(operationID) + ".json")
}

func OperationPrefix() string { return root + "operations/" }

func FileOperationStepPageKey(userID, operationID, stepSetID string, index uint64) objectstore.Key {
	return fixedKey("operation-steps/" + encodedPart(userID) + "/" + encodedPart(operationID) + "/" + encodedPart(stepSetID) + "/" + fmt.Sprintf("%016x.json", index))
}

func FileOperationStepPagePrefix(userID, operationID string) string {
	return root + "operation-steps/" + encodedPart(userID) + "/" + encodedPart(operationID) + "/"
}

func FileOperationStepPageSetPrefix(userID, operationID, stepSetID string) string {
	return FileOperationStepPagePrefix(userID, operationID) + encodedPart(stepSetID) + "/"
}

func FileOperationStepsPrefix() string { return root + "operation-steps/" }

func FileOperationPreparationPageKey(userID, operationID, runSetID string, generation, run, page uint64) objectstore.Key {
	return fixedKey("operation-preparation/" + encodedPart(userID) + "/" + encodedPart(operationID) + "/" + encodedPart(runSetID) + "/" + fmt.Sprintf("%016x/%016x/%016x.json", generation, run, page))
}

func FileOperationPreparationPrefix(userID, operationID string) string {
	return root + "operation-preparation/" + encodedPart(userID) + "/" + encodedPart(operationID) + "/"
}

func FileOperationPreparationsPrefix() string { return root + "operation-preparation/" }

func OperationStagingKey(userID, operationID, artifactID string) objectstore.Key {
	return fixedKey("operation-staging/" + encodedPart(userID) + "/" + encodedPart(operationID) + "/" + encodedPart(artifactID))
}

func OperationStagingPrefix() string { return root + "operation-staging/" }

func IdempotencyKey(userID, key string) objectstore.Key {
	return fixedKey("idempotency/" + encodedPart(userID) + "/" + digestPart(key) + ".json")
}

func IdempotencyPrefix() string { return root + "idempotency/" }

func CheckpointKey(checkpointID string) objectstore.Key {
	return fixedKey("checkpoints/" + encodedPart(checkpointID) + ".json")
}

func CheckpointPrefix() string { return root + "checkpoints/" }

func CheckpointWorkKey(checkpointID, objectKey string) objectstore.Key {
	return fixedKey("checkpoints/" + encodedPart(checkpointID) + "/work/" + digestPart(objectKey) + ".json")
}

func CheckpointWorkPrefix(checkpointID string) string {
	return root + "checkpoints/" + encodedPart(checkpointID) + "/work/"
}

func CheckpointInventoryPageKey(checkpointID string, index uint64) objectstore.Key {
	return fixedKey("checkpoints/" + encodedPart(checkpointID) + "/inventory/" + fmt.Sprintf("%016x.json", index))
}

func CheckpointInventoryPagePrefix(checkpointID string) string {
	return root + "checkpoints/" + encodedPart(checkpointID) + "/inventory/"
}

func GarbageCollectionSessionKey(checkpointID string) objectstore.Key {
	return fixedKey("maintenance/gc/" + encodedPart(checkpointID) + "/session.json")
}

func GarbageCollectionMarkKey(checkpointID, role, targetKey string) objectstore.Key {
	return fixedKey("maintenance/gc/" + encodedPart(checkpointID) + "/marks/" + role + "/" + digestPart(targetKey) + ".json")
}

func GarbageCollectionMarkPrefix(checkpointID string) string {
	return root + "maintenance/gc/" + encodedPart(checkpointID) + "/marks/"
}

func MigrationDirectoryMarkKey(checkpointID, phase, userID, area, directoryID string) objectstore.Key {
	return fixedKey(strings.Join([]string{"maintenance", "migration", encodedPart(checkpointID), phase, encodedPart(userID), area, encodedPart(directoryID) + ".json"}, "/"))
}

func MigrationDirectoryMarkScopePrefix(checkpointID, phase, userID, area string) string {
	return root + strings.Join([]string{"maintenance", "migration", encodedPart(checkpointID), phase, encodedPart(userID), area, ""}, "/")
}

func MigrationDirectoryMarkPrefix(checkpointID string) string {
	return root + strings.Join([]string{"maintenance", "migration", encodedPart(checkpointID), ""}, "/")
}

func LeaseKey(backendKind, leaseID string) objectstore.Key {
	if err := ValidateNamespace(backendKind); err != nil {
		panic(err)
	}
	return fixedKey(fmt.Sprintf("leases/%s/%s.json", backendKind, encodedPart(leaseID)))
}

func LeasePrefix() string { return root + "leases/" }
