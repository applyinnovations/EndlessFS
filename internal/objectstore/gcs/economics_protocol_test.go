package gcs_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	gcstransport "github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"google.golang.org/api/option"
)

func TestGCSTransferEconomicsProtocolCoverage(t *testing.T) {
	ctx := context.Background()
	backend, ledger, dataClient, now := newEconomicsTransferBackend(t)
	key := objectstore.MustKey("endlessfs/v1/fs/economics/blobs/upload")
	handle, err := backend.BeginUpload(ctx, objectstore.UploadRequest{
		UploadID: "economics-upload", Key: key, Size: 4, MediaType: "text/plain", Resumable: true, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEconomicsKinds(t, ledger.Events(), providerbudget.RequestUploadBegin)

	ledger.Reset()
	if _, err := backend.ResumeUpload(ctx, handle.Lease); err != nil {
		t.Fatal(err)
	}
	assertEconomicsKinds(t, ledger.Events())

	ledger.Reset()
	progress, err := backend.UploadProgress(ctx, handle.Lease)
	if err != nil || progress.Complete {
		t.Fatalf("initial UploadProgress() = %+v, %v", progress, err)
	}
	assertEconomicsKinds(t, ledger.Events(), providerbudget.RequestObjectHead, providerbudget.RequestUploadProgress)

	ledger.Reset()
	upload, err := http.NewRequestWithContext(ctx, http.MethodPut, handle.Capability.URL, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	upload.Header.Set("Content-Type", "text/plain")
	upload.Header.Set("Content-Range", "bytes 0-3/4")
	response, err := dataClient.Do(upload)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("direct upload status = %d", response.StatusCode)
	}
	assertEconomicsKinds(t, ledger.Events(), providerbudget.RequestDataUpload)
	assertEconomicsBudget(t, "file-data-upload-four-bytes", ledger.Events())

	ledger.Reset()
	progress, err = backend.UploadProgress(ctx, handle.Lease)
	if err != nil || !progress.Complete || progress.Version == "" {
		t.Fatalf("completed UploadProgress() = %+v, %v", progress, err)
	}
	assertEconomicsKinds(t, ledger.Events(), providerbudget.RequestObjectHead)

	ledger.Reset()
	download, err := backend.CreateDownload(ctx, objectstore.DownloadRequest{
		Key: key, Version: progress.Version, Filename: "upload.txt", MediaType: "text/plain", Disposition: domain.DispositionAttachment, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEconomicsKinds(t, ledger.Events())

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, download.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = dataClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("direct download status = %d", response.StatusCode)
	}
	assertEconomicsKinds(t, ledger.Events(), providerbudget.RequestDataDownload)
	assertEconomicsBudget(t, "file-data-download-four-bytes", ledger.Events())

	abortHandle, err := backend.BeginUpload(ctx, objectstore.UploadRequest{
		UploadID: "economics-abort", Key: objectstore.MustKey("endlessfs/v1/fs/economics/blobs/abort"), Size: 4, MediaType: "text/plain", Resumable: true, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if err := backend.AbortUpload(ctx, abortHandle.Lease); err != nil {
		t.Fatal(err)
	}
	assertEconomicsKinds(t, ledger.Events(), providerbudget.RequestObjectHead, providerbudget.RequestUploadAbort, providerbudget.RequestObjectHead)
}

func assertEconomicsKinds(t *testing.T, events []providerbudget.Event, want ...providerbudget.RequestKind) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("economics events = %+v; want request kinds %v", events, want)
	}
	for index, kind := range want {
		if events[index].Kind != kind {
			t.Fatalf("economics event %d = %+v; want %s", index, events[index], kind)
		}
	}
	model, err := gcstransport.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Estimate(events); err != nil {
		t.Fatalf("economics estimate: %v", err)
	}
}

func assertEconomicsBudget(t *testing.T, name string, events []providerbudget.Event) {
	t.Helper()
	model, err := gcstransport.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcstransport.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	budget, ok := ratchet.Latest(name)
	if !ok {
		t.Fatalf("provider budget %q is missing", name)
	}
	if report, err := budget.CheckRatchet(model, events); err != nil {
		t.Fatalf("%s: %v; observed=%+v", name, err, report.Totals)
	}
}

// TestGCSEconomicsProtocolCoverage measures the actual HTTP emitted by the
// pinned GCS client, rather than assuming that one object-store method always
// equals one wire request. A client-library protocol change must be classified
// and budgeted before it can pass this gate.
func TestGCSEconomicsProtocolCoverage(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		want providerbudget.RequestKind
		run  func(context.Context, *testing.T, *gcstransport.Backend, *providerbudget.Ledger)
	}{
		{name: "head", want: providerbudget.RequestObjectHead, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, ledger *providerbudget.Ledger) {
			key := seedEconomicsObject(ctx, t, backend, ledger, "head")
			if _, err := backend.Head(ctx, key); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "verify", want: providerbudget.RequestObjectHead, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, ledger *providerbudget.Ledger) {
			key := seedEconomicsObject(ctx, t, backend, ledger, "verify")
			if _, err := backend.Verify(ctx, key, objectstore.IntegrityFor([]byte("body"))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "get", want: providerbudget.RequestObjectOpen, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, ledger *providerbudget.Ledger) {
			key := seedEconomicsObject(ctx, t, backend, ledger, "get")
			if _, err := backend.Get(ctx, key); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "open", want: providerbudget.RequestObjectOpen, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, ledger *providerbudget.Ledger) {
			key := seedEconomicsObject(ctx, t, backend, ledger, "open")
			reader, err := backend.Open(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, reader.Body); err != nil {
				t.Fatal(err)
			}
			if err := reader.Body.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "list", want: providerbudget.RequestObjectList, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, _ *providerbudget.Ledger) {
			if _, err := backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/v1/"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "put", want: providerbudget.RequestObjectPut, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, _ *providerbudget.Ledger) {
			if _, err := backend.Put(ctx, economicsKey("put"), []byte("body"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "delete", want: providerbudget.RequestObjectDelete, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, ledger *providerbudget.Ledger) {
			key := economicsKey("delete")
			version, err := backend.Put(ctx, key, []byte("body"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
			if err != nil {
				t.Fatal(err)
			}
			ledger.Reset()
			if err := backend.Delete(ctx, key, objectstore.DeleteCondition{Version: version}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "copy", want: providerbudget.RequestObjectCopy, run: func(ctx context.Context, t *testing.T, backend *gcstransport.Backend, ledger *providerbudget.Ledger) {
			source := economicsKey("copy-source")
			version, err := backend.Put(ctx, source, []byte("body"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
			if err != nil {
				t.Fatal(err)
			}
			ledger.Reset()
			if _, err := backend.Copy(ctx, source, economicsKey("copy-destination"), objectstore.CopyCondition{SourceVersion: version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, ledger := newEconomicsProtocolBackend(t)
			ledger.Reset()
			test.run(ctx, t, backend, ledger)
			events := ledger.Events()
			if len(events) != 1 || events[0].Kind != test.want || events[0].Failed {
				t.Fatalf("wire economics events = %+v; want one successful %s request", events, test.want)
			}
			model, err := gcstransport.RegionalStandardFlatEconomics()
			if err != nil {
				t.Fatal(err)
			}
			maximum, err := model.Estimate(events)
			if err != nil || maximum.Requests != 1 {
				t.Fatalf("wire economics estimate = %+v, %v", maximum, err)
			}
		})
	}
}

func newEconomicsProtocolBackend(t *testing.T) (*gcstransport.Backend, *providerbudget.Ledger) {
	t.Helper()
	server, _ := newGCSServerWithFake(t)
	ledger := providerbudget.NewLedger()
	base := server.Client()
	httpClient := *base
	httpClient.Transport = providerbudget.InstrumentRoundTripper(providerbudget.RoleState, base.Transport, ledger, gcstransport.ClassifyEconomicsRequest)
	client, err := storage.NewClient(context.Background(), storage.WithJSONReads(), option.WithEndpoint(server.URL+"/storage/v1/"), option.WithHTTPClient(&httpClient), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	backend, err := gcstransport.New(client, "endlessfs-test")
	if err != nil {
		t.Fatal(err)
	}
	return backend, ledger
}

func newEconomicsTransferBackend(t *testing.T) (*gcstransport.Backend, *providerbudget.Ledger, *http.Client, time.Time) {
	t.Helper()
	server, _ := newGCSServerWithFake(t)
	ledger := providerbudget.NewLedger()
	base := server.Client()
	controlClient := *base
	controlClient.Transport = providerbudget.InstrumentRoundTripper(providerbudget.RoleFile, base.Transport, ledger, gcstransport.ClassifyEconomicsRequest)
	dataClient := *base
	dataClient.Transport = providerbudget.InstrumentRoundTripper(providerbudget.RoleFileDataPlane, base.Transport, ledger, gcstransport.ClassifyEconomicsRequest)
	client, err := storage.NewClient(context.Background(), storage.WithJSONReads(), option.WithEndpoint(server.URL+"/storage/v1/"), option.WithHTTPClient(&controlClient), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	now := time.Now().UTC()
	backend, err := gcstransport.NewWithTransfers(client, "endlessfs-test", gcstransport.TransferOptions{
		HTTPClient: &controlClient, GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x55}, 256), nil },
		Hostname:  strings.TrimPrefix(server.URL, "http://"), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x44}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x22}, 4096)), Clock: domain.NewFixedClock(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend, ledger, &dataClient, now
}

func seedEconomicsObject(ctx context.Context, t *testing.T, backend *gcstransport.Backend, ledger *providerbudget.Ledger, name string) objectstore.Key {
	t.Helper()
	key := economicsKey(name)
	if _, err := backend.Put(ctx, key, []byte("body"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	return key
}

func economicsKey(name string) objectstore.Key {
	return objectstore.MustKey("endlessfs/v1/state/economics/" + name + ".json")
}
