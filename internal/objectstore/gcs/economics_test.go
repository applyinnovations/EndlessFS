package gcs

import (
	"testing"
)

func TestGCSBudgetRatchetFixture(t *testing.T) {
	ledger, err := RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"direct-move-one-file",
		"namespace-list-page-1000-schema-010",
		"restore-batch-10000-schema-010",
	} {
		if _, ok := ledger.Latest(name); !ok {
			t.Fatalf("%s budget is missing", name)
		}
	}
}
