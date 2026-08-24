package gcs

import (
	"net/http"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestClassifyGCSEconomicsRequest(t *testing.T) {
	for _, test := range []struct {
		name, method, target string
		headers              map[string]string
		want                 providerbudget.RequestKind
	}{
		{name: "metadata", method: http.MethodGet, target: "https://storage.googleapis.test/storage/v1/b/bucket/o/key", want: providerbudget.RequestObjectHead},
		{name: "list", method: http.MethodGet, target: "https://storage.googleapis.test/storage/v1/b/bucket/o?prefix=x", want: providerbudget.RequestObjectList},
		{name: "media", method: http.MethodGet, target: "https://storage.googleapis.test/download/storage/v1/b/bucket/o/key?alt=media", want: providerbudget.RequestObjectOpen},
		{name: "insert", method: http.MethodPost, target: "https://storage.googleapis.test/upload/storage/v1/b/bucket/o?uploadType=multipart", want: providerbudget.RequestObjectPut},
		{name: "delete", method: http.MethodDelete, target: "https://storage.googleapis.test/storage/v1/b/bucket/o/key", want: providerbudget.RequestObjectDelete},
		{name: "rewrite", method: http.MethodPost, target: "https://storage.googleapis.test/storage/v1/b/source/o/a/rewriteTo/b/destination/o/b", want: providerbudget.RequestObjectCopy},
		{name: "begin", method: http.MethodPost, target: "https://storage.googleapis.test/bucket/key", headers: map[string]string{"x-goog-resumable": "start"}, want: providerbudget.RequestUploadBegin},
		{name: "status", method: http.MethodPut, target: "https://storage.googleapis.test/upload/session", headers: map[string]string{"Content-Range": "bytes */100"}, want: providerbudget.RequestUploadProgress},
		{name: "chunk", method: http.MethodPut, target: "https://storage.googleapis.test/upload/session", headers: map[string]string{"Content-Range": "bytes 0-99/100"}, want: providerbudget.RequestDataUpload},
		{name: "abort", method: http.MethodDelete, target: "https://storage.googleapis.test/upload/session", want: providerbudget.RequestUploadAbort},
		{name: "download", method: http.MethodGet, target: "https://storage.googleapis.test/bucket/key?X-Goog-Signature=x", want: providerbudget.RequestDataDownload},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, test.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			got, err := ClassifyEconomicsRequest(request)
			if err != nil || got != test.want {
				t.Fatalf("ClassifyEconomicsRequest() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestClassifyGCSEconomicsRequestFailsClosed(t *testing.T) {
	request, _ := http.NewRequest(http.MethodPatch, "https://storage.googleapis.test/new-api", nil)
	if kind, err := ClassifyEconomicsRequest(request); err == nil || kind != providerbudget.RequestUnclassified {
		t.Fatalf("ClassifyEconomicsRequest() = %q, %v", kind, err)
	}
}
