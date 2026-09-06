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

func TestSchema009StateRecordCodecPreservesOpaqueStatePayload(t *testing.T) {
	payload := []byte{0, 1, 2, 0xff, 'n', 'o', 't', '-', 'j', 's', 'o', 'n'}
	body, err := EncodeStateRecord009(StateRecordSession, payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStateRecord009(body, StateRecordSession)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded = %x, %v; want %x", decoded, err, payload)
	}
}

func TestSchema009StateRecordCodecRejectsNonCanonicalAndMalformedBodies(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty-payload":   []byte(`{"schemaVersion":1,"recordType":"account","payload":null}`),
		"duplicate-field": []byte(`{"schemaVersion":1,"recordType":"account","recordType":"session","payload":"e30="}`),
		"unknown-field":   []byte(`{"schemaVersion":1,"recordType":"account","payload":"e30=","extra":true}`),
		"wrong-version":   []byte(`{"schemaVersion":2,"recordType":"account","payload":"e30="}`),
		"trailing":        []byte(`{"schemaVersion":1,"recordType":"account","payload":"e30="} {}`),
		"non-canonical":   []byte(`{ "schemaVersion":1,"recordType":"account","payload":"e30=" }`),
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
