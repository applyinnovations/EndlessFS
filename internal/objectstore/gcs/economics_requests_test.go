package gcs

import (
	"net/http"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestClassifyGCSEconomicsRequestFailsClosed(t *testing.T) {
	if kind, err := ClassifyEconomicsRequest(nil); err == nil || kind != providerbudget.RequestUnclassified {
		t.Fatalf("ClassifyEconomicsRequest(nil) = %q, %v", kind, err)
	}
	request, _ := http.NewRequest(http.MethodPatch, "https://storage.googleapis.test/new-api", nil)
	if kind, err := ClassifyEconomicsRequest(request); err == nil || kind != providerbudget.RequestUnclassified {
		t.Fatalf("ClassifyEconomicsRequest() = %q, %v", kind, err)
	}
}
