package objectstorecontract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type UploadAbortBackend interface {
	objectstore.Backend
	objectstore.DirectTransferBackend
}

type UploadAbortHarness struct {
	Backend UploadAbortBackend
	Client  *http.Client
	Now     time.Time
}

// RunUploadAbort proves revocation of active capabilities and conservation of
// finalized objects that a concurrent namespace publication may already use.
func RunUploadAbort(t *testing.T, factory func(*testing.T) UploadAbortHarness) {
	t.Helper()
	for _, resumable := range []bool{false, true} {
		for _, finalized := range []bool{false, true} {
			t.Run(fmt.Sprintf("resumable=%t/finalized=%t", resumable, finalized), func(t *testing.T) {
				h := factory(t)
				ctx := context.Background()
				key := objectstore.MustKey("endlessfs/v1/fs/abort-contract/blobs/object")
				body := []byte("data")
				handle, err := h.Backend.BeginUpload(ctx, objectstore.UploadRequest{
					UploadID: "abort-contract", Key: key, Size: int64(len(body)), MediaType: "application/octet-stream",
					Resumable: resumable, ExpiresAt: h.Now.Add(time.Hour),
				})
				if err != nil {
					t.Fatal(err)
				}
				upload := func() int {
					request, err := http.NewRequestWithContext(ctx, handle.Capability.Method, handle.Capability.URL, bytes.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					for key, value := range handle.Capability.Headers {
						request.Header.Set(key, value)
					}
					if handle.Capability.Framing == domain.UploadFramingContentRange {
						request.Header.Set("Content-Range", "bytes 0-3/4")
					}
					response, err := h.Client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					if err := response.Body.Close(); err != nil {
						t.Fatal(err)
					}
					return response.StatusCode
				}
				var original objectstore.ObjectInfo
				if finalized {
					if status := upload(); status < 200 || status >= 300 {
						t.Fatalf("upload status = %d", status)
					}
					original, err = h.Backend.Verify(ctx, key, objectstore.IntegrityFor(body))
					if err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					if err := h.Backend.AbortUpload(ctx, handle.Lease); err != nil && !errors.Is(err, domain.ErrNotFound) {
						t.Fatal(err)
					}
				}
				if !finalized {
					if status := upload(); status < 400 {
						t.Fatalf("aborted capability accepted upload: %d", status)
					}
					if _, err := h.Backend.Head(ctx, key); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("aborted active upload created an object: %v", err)
					}
					return
				}
				preserved, err := h.Backend.Verify(ctx, key, objectstore.IntegrityFor(body))
				if err != nil || preserved != original {
					t.Fatalf("abort changed finalized object: before=%+v after=%+v error=%v", original, preserved, err)
				}
				reader, err := h.Backend.Open(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(reader.Body)
				closeErr := reader.Body.Close()
				if readErr != nil || closeErr != nil || !bytes.Equal(got, body) {
					t.Fatalf("preserved bytes = %q, read=%v close=%v", got, readErr, closeErr)
				}
			})
		}
	}
}
