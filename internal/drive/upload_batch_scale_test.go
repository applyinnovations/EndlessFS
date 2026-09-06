package drive_test

import (
	"testing"

	"github.com/applyinnovations/endlessfs/internal/drive"
)

// The browser and provider contract must share one product-scale envelope.
// Keeping this assertion beside the executable provider-budget workflow makes
// a regression back to repeated 100-item control transactions fail before any
// cloud-backed benchmark is run.
func TestUploadBatchProductCardinalityIsTenThousand(t *testing.T) {
	if drive.MaxUploadBatchItems != 10_000 {
		t.Fatalf("MaxUploadBatchItems = %d, want 10000", drive.MaxUploadBatchItems)
	}
}
