package storageformat

import (
	"bytes"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestSchema009StateRecordCodecBindsCanonicalTypeAndPayload(t *testing.T) {
	payload := []byte(`{"schemaVersion":1,"userID":"WVhXWVhXWVhXWVhXWVhXWQ","status":"enabled"}`)
	body, err := EncodeStateRecord009(StateRecordAccount, payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStateRecord009(body, StateRecordAccount)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded = %s, %v", decoded, err)
	}
	if _, err := DecodeStateRecord009(body, StateRecordSession); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("wrong record type error = %v", err)
	}
}

func TestSchema009StateRecordCodecRejectsNonCanonicalAndMalformedBodies(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty-payload":    []byte(`{"schemaVersion":1,"recordType":"account","payload":null}`),
		"duplicate-field": []byte(`{"schemaVersion":1,"recordType":"account","recordType":"session","payload":{}}`),
		"unknown-field":   []byte(`{"schemaVersion":1,"recordType":"account","payload":{},"extra":true}`),
		"wrong-version":   []byte(`{"schemaVersion":2,"recordType":"account","payload":{}}`),
		"trailing":        []byte(`{"schemaVersion":1,"recordType":"account","payload":{}} {}`),
		"non-canonical":   []byte(`{ "schemaVersion":1,"recordType":"account","payload":{} }`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeStateRecord009(body, StateRecordAccount); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := EncodeStateRecord009("", []byte(`{}`)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty type error = %v", err)
	}
	if _, err := EncodeStateRecord009(StateRecordAccount, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty payload error = %v", err)
	}
}
