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

func TestProtocolErrorsMapToStableSafeKinds(t *testing.T) {
	backend, fake := newProtocolBackend(t)
	key := objectstore.MustKey("endlessfs/v1/state/users/fault.json")
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
		key = objectstore.MustKey("endlessfs/v1/state/users/fault-" + string(rune('a'+index)) + ".json")
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
