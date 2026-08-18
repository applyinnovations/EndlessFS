package integrity_test

import (
	"testing"

	"github.com/applyinnovations/endlessfs/internal/integrity"
)

func TestCRC32CCanonicalEncoding(t *testing.T) {
	encoded := integrity.CRC32C([]byte("123456789"))
	value, ok := integrity.ParseCRC32C(encoded)
	if !ok || value != 0xe3069283 {
		t.Fatalf("CRC32C = %q, %#x, %v", encoded, value, ok)
	}
	for _, invalid := range []string{"", "invalid", encoded + "=", "AAAA"} {
		if _, accepted := integrity.ParseCRC32C(invalid); accepted {
			t.Errorf("ParseCRC32C(%q) accepted", invalid)
		}
	}
}
