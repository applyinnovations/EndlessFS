package gcs_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	gcstransport "github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/objectstore/objectstorecontract"
	"google.golang.org/api/option"
)

func TestContractGCSProtocol(t *testing.T) {
	objectstorecontract.Run(t, func(t *testing.T) objectstore.Backend {
		backend, _ := newProtocolBackend(t)
		return backend
	})
}

func TestLostUploadSuccessIsUnavailableAndNotRetried(t *testing.T) {
	backend, fake := newProtocolBackend(t)
	fake.mu.Lock()
	fake.failUploadAfterCommit = true
	fake.mu.Unlock()
	key := objectstore.MustKey("endlessfs/v1/state/users/lost-success.json")
	if _, err := backend.Put(context.Background(), key, []byte("committed"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Put() error = %v", err)
	}
	fake.mu.Lock()
	requests := fake.uploadRequests
	fake.mu.Unlock()
	if requests != 1 {
		t.Fatalf("upload requests = %d, want one transport attempt", requests)
	}
	got, err := backend.Get(context.Background(), key)
	if err != nil || string(got.Body) != "committed" {
		t.Fatalf("Get() after lost success = %+v, %v", got, err)
	}
}

func TestServerWrittenObjectsSetNoStoreMetadata(t *testing.T) {
	backend, fake := newProtocolBackend(t)
	key := objectstore.MustKey("endlessfs/v1/state/users/cache-policy.json")
	if _, err := backend.Put(context.Background(), key, []byte("private"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	cacheControl := fake.objects[key.String()].cacheControl
	fake.mu.Unlock()
	if cacheControl != "no-store" {
		t.Fatalf("server-written object cache control = %q", cacheControl)
	}
}

func TestProtocolErrorsMapToStableSafeKinds(t *testing.T) {
	backend, fake := newProtocolBackend(t)
	tests := []struct {
		status int
		want   error
	}{
		{400, domain.ErrInvalid}, {401, domain.ErrUnauthenticated}, {403, domain.ErrUnauthorized},
		{404, domain.ErrNotFound}, {409, domain.ErrConflict}, {412, domain.ErrConflict},
		{429, domain.ErrRateLimited}, {500, domain.ErrUnavailable}, {503, domain.ErrUnavailable},
	}
	for index, test := range tests {
		fake.mu.Lock()
		fake.nextStatus = test.status
		fake.mu.Unlock()
		key := objectstore.MustKey("endlessfs/v1/state/users/fault-" + string(rune('a'+index)) + ".json")
		_, err := backend.Put(context.Background(), key, []byte("fault"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if !errors.Is(err, test.want) {
			t.Errorf("status %d error = %v, want %v", test.status, err, test.want)
		}
		if err != nil && (strings.Contains(err.Error(), key.String()) || strings.Contains(err.Error(), "injected")) {
			t.Errorf("status %d exposed provider detail: %v", test.status, err)
		}
	}
}

func TestChecksumsSizesListingsAndCursorsFailClosed(t *testing.T) {
	backend, fake := newProtocolBackend(t)
	key := objectstore.MustKey("endlessfs/v1/state/users/integrity.json")
	if _, err := backend.Put(context.Background(), key, []byte("integrity"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.corruptNextDownloadCRC = true
	fake.mu.Unlock()
	if _, err := backend.Get(context.Background(), key); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("corrupt checksum error = %v", err)
	}
	fake.mu.Lock()
	fake.wrongNextMetadataSizeBy = 1
	fake.mu.Unlock()
	if _, err := backend.Get(context.Background(), key); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("wrong size error = %v", err)
	}
	fake.mu.Lock()
	fake.objects["endlessfs/v1/\x00invalid"] = fakeObject{body: []byte("x"), generation: 999, metageneration: 1}
	fake.mu.Unlock()
	if _, err := backend.List(context.Background(), objectstore.ListRequest{Prefix: "endlessfs/v1/"}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("invalid listed key error = %v", err)
	}
	if _, err := backend.List(context.Background(), objectstore.ListRequest{Prefix: "endlessfs/v1/state/", Cursor: "not a GCS token!"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid cursor error = %v", err)
	}
}

func TestVerifyUsesProviderIntegrityMetadataWithoutReadingObjectBytes(t *testing.T) {
	backend, fake := newProtocolBackend(t)
	key := objectstore.MustKey("endlessfs/v1/state/users/metadata-integrity.json")
	body := []byte("metadata-verified")
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.corruptNextDownloadCRC = true
	fake.mu.Unlock()
	if _, err := backend.Verify(context.Background(), key, objectstore.IntegrityFor(body)); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if _, err := backend.Get(context.Background(), key); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("Verify consumed object bytes instead of metadata; following Get() error = %v", err)
	}
}

func TestGenerationConditionsFenceEveryMutation(t *testing.T) {
	backend, _ := newProtocolBackend(t)
	ctx := context.Background()
	source := objectstore.MustKey("endlessfs/v1/staging/user/op/source")
	destination := objectstore.MustKey("endlessfs/v1/fs/user/blobs/destination")
	version, err := backend.Put(ctx, source, []byte("body"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		t.Fatal(err)
	}
	stale := objectstore.NativeVersion("gcs-v1.999999")
	if _, err := backend.Put(ctx, source, []byte("changed"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: stale}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale Put() error = %v", err)
	}
	if _, err := backend.Copy(ctx, source, destination, objectstore.CopyCondition{SourceVersion: stale, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale-source Copy() error = %v", err)
	}
	if _, err := backend.Get(ctx, destination); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale copy created destination: %v", err)
	}
	if _, err := backend.Copy(ctx, source, destination, objectstore.CopyCondition{SourceVersion: version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Copy(ctx, source, destination, objectstore.CopyCondition{SourceVersion: version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate destination Copy() error = %v", err)
	}
	if err := backend.Delete(ctx, source, objectstore.DeleteCondition{Version: stale}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale Delete() error = %v", err)
	}
}

func TestInputAndContextBoundaries(t *testing.T) {
	backend, _ := newProtocolBackend(t)
	if _, err := gcstransport.New(nil, "endlessfs-test"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("New(nil) error = %v", err)
	}
	if _, err := gcstransport.New(backendClient(t), "x"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("New(short bucket) error = %v", err)
	}
	if _, err := backend.List(context.Background(), objectstore.ListRequest{Prefix: "invalid", Limit: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List(invalid prefix) error = %v", err)
	}
	if _, err := backend.List(context.Background(), objectstore.ListRequest{Prefix: "endlessfs/v1/", Limit: 1001}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List(invalid limit) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Get(cancelled, objectstore.MustKey("endlessfs/v1/state/users/cancelled.json")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Get(cancelled) error = %v", err)
	}
	expected := objectstore.IntegrityFor([]byte("expected"))
	if _, err := backend.Verify(cancelled, objectstore.MustKey("endlessfs/v1/state/users/cancelled-verify.json"), expected); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Verify(cancelled) error = %v", err)
	}
	if _, err := backend.Verify(context.Background(), objectstore.Key{}, expected); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Verify(invalid key) error = %v", err)
	}
	if _, err := backend.Verify(context.Background(), objectstore.MustKey("endlessfs/v1/state/users/missing-verify.json"), expected); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Verify(missing) error = %v", err)
	}
}

func TestEveryGCSOperationFailsClosedAtItsBoundary(t *testing.T) {
	backend, fake := newProtocolBackend(t)
	ctx := context.Background()
	key := objectstore.MustKey("endlessfs/v1/state/users/boundary.json")
	destination := objectstore.MustKey("endlessfs/v1/state/users/destination.json")
	version, err := backend.Put(ctx, key, []byte("value"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := backend.Head(canceled, key); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Head error = %v", err)
	}
	if _, err := backend.List(canceled, objectstore.ListRequest{Prefix: "endlessfs/v1/"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled List error = %v", err)
	}
	if _, err := backend.Put(canceled, key, nil, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Put error = %v", err)
	}
	if err := backend.Delete(canceled, key, objectstore.DeleteCondition{Version: version}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Delete error = %v", err)
	}
	if _, err := backend.Copy(canceled, key, destination, objectstore.CopyCondition{SourceVersion: version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Copy error = %v", err)
	}

	var invalidKey objectstore.Key
	if _, err := backend.Head(ctx, invalidKey); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Head error = %v", err)
	}
	if _, err := backend.Get(ctx, invalidKey); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Get error = %v", err)
	}
	if _, err := backend.Put(ctx, invalidKey, nil, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Put error = %v", err)
	}
	if err := backend.Delete(ctx, invalidKey, objectstore.DeleteCondition{Version: version}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Delete error = %v", err)
	}
	if err := backend.Delete(ctx, key, objectstore.DeleteCondition{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("versionless Delete error = %v", err)
	}
	if _, err := backend.Copy(ctx, invalidKey, destination, objectstore.CopyCondition{SourceVersion: version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Copy error = %v", err)
	}
	if _, err := backend.Copy(ctx, key, destination, objectstore.CopyCondition{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("versionless Copy error = %v", err)
	}
	if _, err := backend.Copy(ctx, key, destination, objectstore.CopyCondition{SourceVersion: "foreign", Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("foreign Copy version error = %v", err)
	}

	for operation, invoke := range map[string]func() error{
		"head": func() error { _, err := backend.Head(ctx, key); return err },
		"get":  func() error { _, err := backend.Get(ctx, key); return err },
		"list": func() error {
			_, err := backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/v1/"})
			return err
		},
		"delete": func() error { return backend.Delete(ctx, key, objectstore.DeleteCondition{Version: version}) },
	} {
		fake.mu.Lock()
		fake.nextStatus = 418
		fake.mu.Unlock()
		if err := invoke(); !errors.Is(err, domain.ErrInternal) || strings.Contains(err.Error(), key.String()) {
			t.Fatalf("%s provider error = %v", operation, err)
		}
	}

	page, err := backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/v1/"})
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Version != version {
		t.Fatalf("default List = %+v, %v", page, err)
	}
	if err := backend.Delete(ctx, key, objectstore.DeleteCondition{Version: "foreign"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("foreign Delete version error = %v", err)
	}
}

func newProtocolBackend(t *testing.T) (*gcstransport.Backend, *fakeGCS) {
	t.Helper()
	server, fake := newGCSServerWithFake(t)
	client := protocolClient(t, server)
	backend, err := gcstransport.New(client, "endlessfs-test")
	if err != nil {
		t.Fatal(err)
	}
	return backend, fake
}

func backendClient(t *testing.T) *storage.Client {
	t.Helper()
	server := newGCSServer(t)
	return protocolClient(t, server)
}

func protocolClient(t *testing.T, server *httptest.Server) *storage.Client {
	t.Helper()
	client, err := storage.NewClient(context.Background(), storage.WithJSONReads(), option.WithEndpoint(server.URL+"/storage/v1/"), option.WithHTTPClient(server.Client()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
