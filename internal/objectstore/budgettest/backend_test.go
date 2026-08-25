package budgettest

import (
	"context"
	"io"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestInstrumentedBackendRecordsEveryPrimitiveByRole(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	ledger := providerbudget.NewLedger()
	backend := Wrap(providerbudget.RolePreviewArtifact, base, ledger)
	key := objectstore.MustKey("endlessfs/v1/preview/test")
	version, err := backend.Put(ctx, key, []byte("artifact"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		t.Fatal(err)
	}
	info, err := backend.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Verify(ctx, key, objectstore.ExpectedIntegrity{Size: 8, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: info.Fingerprint.CRC32C}}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	stream, err := backend.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, stream.Body); err != nil {
		t.Fatal(err)
	}
	if err := stream.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/v1/preview/", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	copyKey := objectstore.MustKey("endlessfs/v1/preview/copy")
	copyResult, err := backend.Copy(ctx, key, copyKey, objectstore.CopyCondition{SourceVersion: version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(ctx, copyKey, objectstore.DeleteCondition{Version: copyResult.Version}); err != nil {
		t.Fatal(err)
	}

	events := ledger.Events()
	want := []providerbudget.RequestKind{providerbudget.RequestObjectPut, providerbudget.RequestObjectHead, providerbudget.RequestObjectVerify, providerbudget.RequestObjectGet, providerbudget.RequestObjectOpen, providerbudget.RequestObjectList, providerbudget.RequestObjectCopy, providerbudget.RequestObjectDelete}
	if len(events) != len(want) {
		t.Fatalf("events = %+v", events)
	}
	for index, kind := range want {
		if events[index].Kind != kind || events[index].Role != providerbudget.RolePreviewArtifact {
			t.Fatalf("event %d = %+v, want %s/%s", index, events[index], providerbudget.RolePreviewArtifact, kind)
		}
	}
	if events[0].RequestBytes != 8 || events[3].ResponseBytes != 8 || events[4].ResponseBytes != 8 {
		t.Fatalf("byte accounting = %+v", events)
	}
	ledger.Reset()
	if len(ledger.Events()) != 0 {
		t.Fatal("Reset() retained events")
	}
}
