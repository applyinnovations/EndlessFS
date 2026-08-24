package providerbudget

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecordingRoundTripperCountsWireRequestsAndBodyBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(append([]byte("reply:"), body...))
	}))
	defer server.Close()
	ledger := NewLedger()
	client := server.Client()
	client.Transport = InstrumentRoundTripper(RoleFileDataPlane, client.Transport, ledger, func(request *http.Request) (RequestKind, error) {
		return RequestDataUpload, nil
	})
	request, err := http.NewRequest(http.MethodPut, server.URL+"/session", bytes.NewBufferString("payload"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	events := ledger.Events()
	if len(events) != 1 || events[0].Kind != RequestDataUpload || events[0].RequestBytes != 7 || events[0].ResponseBytes != 13 || events[0].StatusCode != http.StatusOK {
		t.Fatalf("wire events = %+v", events)
	}
}
