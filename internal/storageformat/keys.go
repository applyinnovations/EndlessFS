package storageformat

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strconv"
	"strings"

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

func StateVersionKey(namespace, logicalKey, logicalVersion string) objectstore.Key {
	if err := ValidateNamespace(namespace); err != nil {
		panic(err)
	}
	return fixedKey("state-versions/" + namespace + "/" + digestPart(logicalKey) + "/" + digestPart(logicalVersion) + ".json")
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

func OperationKey(userID, operationID string) objectstore.Key {
	return fixedKey("operations/" + encodedPart(userID) + "/" + encodedPart(operationID) + ".json")
}

func OperationPrefix() string { return root + "operations/" }

func IdempotencyKey(userID, key string) objectstore.Key {
	return fixedKey("idempotency/" + encodedPart(userID) + "/" + digestPart(key) + ".json")
}

func CheckpointKey(checkpointID string) objectstore.Key {
	return fixedKey("checkpoints/" + encodedPart(checkpointID) + ".json")
}

func LeaseKey(backendKind, leaseID string) objectstore.Key {
	if err := ValidateNamespace(backendKind); err != nil {
		panic(err)
	}
	return fixedKey(fmt.Sprintf("leases/%s/%s.json", backendKind, encodedPart(leaseID)))
}
