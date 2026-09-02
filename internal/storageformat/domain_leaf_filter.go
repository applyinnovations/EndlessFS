package storageformat

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const (
	DomainLeafKeyFilterBytes  = 512
	domainLeafKeyFilterHashes = 8
)

// DomainLeafKeyFilter constructs a bounded authenticated membership hint for
// one leaf page. A negative answer is definitive; a positive answer always
// falls through to the canonical leaf and can therefore only cost an extra
// read, never manufacture membership.
func DomainLeafKeyFilter(keys []string) string {
	bits := make([]byte, DomainLeafKeyFilterBytes)
	for _, key := range keys {
		digest := sha256.Sum256([]byte("endlessfs-domain-leaf-key-filter-v1\x00" + key))
		for index := 0; index < domainLeafKeyFilterHashes; index++ {
			position := binary.BigEndian.Uint32(digest[index*4:(index+1)*4]) % uint32(len(bits)*8)
			bits[position/8] |= byte(1 << (position % 8))
		}
	}
	return base64.RawURLEncoding.EncodeToString(bits)
}

func ValidateDomainLeafKeyFilter(value string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != DomainLeafKeyFilterBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain leaf key filter")
	}
	return nil
}

func DomainLeafKeyFilterMayContain(value, key string) (bool, error) {
	if err := ValidateDomainLeafKeyFilter(value); err != nil {
		return false, err
	}
	bits, _ := base64.RawURLEncoding.DecodeString(value)
	digest := sha256.Sum256([]byte("endlessfs-domain-leaf-key-filter-v1\x00" + key))
	for index := 0; index < domainLeafKeyFilterHashes; index++ {
		position := binary.BigEndian.Uint32(digest[index*4:(index+1)*4]) % uint32(len(bits)*8)
		if bits[position/8]&(byte(1<<(position%8))) == 0 {
			return false, nil
		}
	}
	return true, nil
}
