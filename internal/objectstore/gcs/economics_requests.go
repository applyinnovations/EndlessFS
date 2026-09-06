package gcs

import (
	"errors"
	"net/http"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

// ClassifyEconomicsRequest maps an actual GCS protocol request to the reviewed
// economics vocabulary. Unknown requests fail closed so a client-library
// change cannot silently escape count, cost, or latency budgets.
func ClassifyEconomicsRequest(request *http.Request) (providerbudget.RequestKind, error) {
	if request == nil || request.URL == nil {
		return providerbudget.RequestUnclassified, errors.New("GCS economics request is missing")
	}
	path := request.URL.EscapedPath()
	query := request.URL.Query()
	if strings.EqualFold(request.Header.Get("x-goog-resumable"), "start") {
		return providerbudget.RequestUploadBegin, nil
	}
	if contentRange := request.Header.Get("Content-Range"); contentRange != "" {
		if strings.HasPrefix(contentRange, "bytes */") {
			return providerbudget.RequestUploadProgress, nil
		}
		return providerbudget.RequestDataUpload, nil
	}
	if strings.Contains(path, "/rewriteTo/") {
		return providerbudget.RequestObjectCopy, nil
	}
	if strings.Contains(path, "/upload/storage/v1/") || query.Get("uploadType") != "" {
		return providerbudget.RequestObjectPut, nil
	}
	if request.Method == http.MethodDelete {
		if !strings.Contains(path, "/storage/v1/") || strings.Contains(path, "/upload/") || query.Has("upload_id") || query.Has("uploadId") {
			return providerbudget.RequestUploadAbort, nil
		}
		return providerbudget.RequestObjectDelete, nil
	}
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		if query.Get("alt") == "media" || strings.Contains(path, "/download/storage/v1/") {
			return providerbudget.RequestObjectOpen, nil
		}
		if !strings.Contains(path, "/storage/v1/") {
			return providerbudget.RequestDataDownload, nil
		}
		if strings.HasSuffix(strings.TrimSuffix(path, "/"), "/o") {
			return providerbudget.RequestObjectList, nil
		}
		return providerbudget.RequestObjectHead, nil
	}
	return providerbudget.RequestUnclassified, errors.New("GCS protocol request has no economics classification")
}
