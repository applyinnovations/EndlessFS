package storageformat

import (
	"fmt"
	"testing"
)

func TestDomainLeafKeyFilterHasNoFalseNegativesAndRejectsMalformedValues(t *testing.T) {
	keys := make([]string, 256)
	for index := range keys {
		keys[index] = fmt.Sprintf("key-%03d", index)
	}
	filter := DomainLeafKeyFilter(keys)
	if err := ValidateDomainLeafKeyFilter(filter); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		present, err := DomainLeafKeyFilterMayContain(filter, key)
		if err != nil || !present {
			t.Fatalf("filter lost %q: present=%t err=%v", key, present, err)
		}
	}
	for _, malformed := range []string{"", "not-base64", filter[:len(filter)-1]} {
		if err := ValidateDomainLeafKeyFilter(malformed); err == nil {
			t.Fatalf("ValidateDomainLeafKeyFilter(%q) succeeded", malformed)
		}
	}
}
