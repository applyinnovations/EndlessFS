package state

import (
	"strings"
	"testing"
)

type testRecord struct {
	SchemaVersion int               `json:"schemaVersion"`
	Name          string            `json:"name"`
	Nested        map[string]string `json:"nested,omitempty"`
}

func (r *testRecord) Validate() error {
	if r.SchemaVersion != 1 {
		return errTestValidation
	}
	return nil
}

func TestStrictJSONRoundTrip(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeJSON(&testRecord{SchemaVersion: 1, Name: "value"})
	if err != nil {
		t.Fatalf("EncodeJSON() error = %v", err)
	}
	var decoded testRecord
	if err := DecodeJSON(encoded, &decoded); err != nil {
		t.Fatalf("DecodeJSON() error = %v", err)
	}
	if decoded.Name != "value" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestStrictJSONRejectsMalformedRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "unknown field", data: []byte(`{"schemaVersion":1,"name":"x","extra":true}`)},
		{name: "duplicate field", data: []byte(`{"schemaVersion":1,"name":"x","name":"y"}`)},
		{name: "nested duplicate", data: []byte(`{"schemaVersion":1,"name":"x","nested":{"a":"x","a":"y"}}`)},
		{name: "trailing", data: []byte(`{"schemaVersion":1,"name":"x"} {}`)},
		{name: "invalid utf8", data: []byte{'{', '"', 0xff, '"', ':', '1', '}'}},
		{name: "invalid schema", data: []byte(`{"schemaVersion":2,"name":"x"}`)},
		{name: "oversized", data: []byte(`{"schemaVersion":1,"name":"` + strings.Repeat("a", MaxRecordBytes) + `"}`)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var record testRecord
			if err := DecodeJSON(test.data, &record); err == nil {
				t.Fatal("DecodeJSON() accepted invalid record")
			}
		})
	}
}

var errTestValidation = &validationError{}

type validationError struct{}

func (*validationError) Error() string { return "invalid test schema" }
