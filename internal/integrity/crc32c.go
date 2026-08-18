// Package integrity provides provider-independent integrity encodings shared
// by application manifests and object-store transport assertions.
package integrity

import (
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// CRC32C returns a canonical unpadded base64url encoding of the Castagnoli
// checksum. This encoding is an EndlessFS value, not provider metadata.
func CRC32C(data []byte) string {
	encoded := make([]byte, 4)
	binary.BigEndian.PutUint32(encoded, crc32.Checksum(data, castagnoliTable))
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// ParseCRC32C accepts only the canonical encoding returned by CRC32C.
func ParseCRC32C(value string) (uint32, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 4 || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return 0, false
	}
	return binary.BigEndian.Uint32(decoded), true
}
