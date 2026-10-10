package gcs

import (
	"io"
	"net/http"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

// observedTransport counts actual wire attempts, including SDK retries, using
// the reviewed request-kind classifier. No raw URL, key or header is retained.
type observedTransport struct{ base http.RoundTripper }

func (transport observedTransport) CloseIdleConnections() {
	if closer, ok := transport.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (transport observedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	kind, _ := ClassifyEconomicsRequest(request)
	operation := telemetry.WireUnknown
	switch kind {
	case providerbudget.RequestObjectHead:
		operation = telemetry.WireHead
	case providerbudget.RequestObjectOpen:
		operation = telemetry.WireRead
	case providerbudget.RequestObjectList:
		operation = telemetry.WireList
	case providerbudget.RequestObjectPut:
		operation = telemetry.WirePut
	case providerbudget.RequestObjectDelete:
		operation = telemetry.WireDelete
	case providerbudget.RequestObjectCopy:
		operation = telemetry.WireCopy
	case providerbudget.RequestUploadBegin:
		operation = telemetry.WireUploadBegin
	case providerbudget.RequestUploadProgress:
		operation = telemetry.WireUploadStatus
	case providerbudget.RequestUploadAbort:
		operation = telemetry.WireUploadAbort
	case providerbudget.RequestDataUpload:
		operation = telemetry.WireUploadData
	case providerbudget.RequestDataDownload:
		operation = telemetry.WireDownloadData
	}
	ctx, activity := telemetry.Start(request.Context(), operation, telemetry.ContextRole(request.Context()))
	response, err := transport.base.RoundTrip(request.WithContext(ctx))
	if err != nil {
		activity.End(err)
		return response, err
	}
	result := telemetry.Success
	switch {
	case response.StatusCode == 401 || response.StatusCode == 403:
		result = telemetry.Denied
	case response.StatusCode == 404:
		result = telemetry.NotFound
	case response.StatusCode == 409 || response.StatusCode == 412:
		result = telemetry.Conflict
	case response.StatusCode == 408 || response.StatusCode == 504:
		result = telemetry.Timeout
	case response.StatusCode == 429 || response.StatusCode >= 500:
		result = telemetry.Unavailable
	case response.StatusCode >= 400:
		result = telemetry.Invalid
	}
	activity.Bytes(0, request.ContentLength)
	if activity == nil {
		return response, nil
	}
	response.Body = &observedResponseBody{ReadCloser: response.Body, activity: activity, result: result}
	return response, nil
}

type observedResponseBody struct {
	io.ReadCloser
	activity *telemetry.Activity
	result   telemetry.Result
	once     sync.Once
}

func (body *observedResponseBody) Read(target []byte) (int, error) {
	count, err := body.ReadCloser.Read(target)
	body.activity.Bytes(int64(count), 0)
	if err != nil && err != io.EOF {
		body.once.Do(func() { body.activity.End(err) })
	}
	return count, err
}
func (body *observedResponseBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(func() {
		if err != nil {
			body.activity.End(err)
		} else {
			body.activity.EndResult(body.result)
		}
	})
	return err
}
