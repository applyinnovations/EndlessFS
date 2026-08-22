// Package integrity provides provider-independent integrity encodings shared
// by application manifests and object-store transport assertions.
package integrity

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// CRC32C returns a canonical unpadded base64url encoding of the Castagnoli
// checksum. This encoding is an EndlessFS value, not provider metadata.
func CRC32C(data []byte) string {
	return EncodeCRC32C(crc32.Checksum(data, castagnoliTable))
}

// EncodeCRC32C normalizes a provider-returned numeric Castagnoli checksum.
func EncodeCRC32C(value uint32) string {
	encoded := make([]byte, 4)
	binary.BigEndian.PutUint32(encoded, value)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// MD5 is used by deterministic local backends and tests. Production file
// fingerprints come from provider metadata and never from service-side reads.
func MD5(data []byte) string {
	digest := md5.Sum(data)
	return EncodeMD5(digest[:])
}

func EncodeMD5(value []byte) string {
	if len(value) != md5.Size {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func ParseMD5(value string) ([md5.Size]byte, bool) {
	var result [md5.Size]byte
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != len(result) || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return result, false
	}
	copy(result[:], decoded)
	return result, true
}

// ParseCRC32C accepts only the canonical encoding returned by CRC32C.
func ParseCRC32C(value string) (uint32, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 4 || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return 0, false
	}
	return binary.BigEndian.Uint32(decoded), true
}
