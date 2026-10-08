package portable

import (
	"encoding/json"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

// New sample sizes are measured before their append-only ratchet is recorded.
// Missing or changed measurements still fail the gate.
func checkGrowthBudget(t *testing.T, ratchet providerbudget.RatchetLedger, name string, model providerbudget.Model, roles []providerbudget.Role, events []providerbudget.Event) (providerbudget.Report, error) {
	t.Helper()
	budget, err := providerbudget.Calibrate(name, model, roles, events)
	if err != nil {
		return providerbudget.Report{}, err
	}
	body, err := json.Marshal(budget)
	if err != nil {
		return providerbudget.Report{}, err
	}
	t.Logf("GROWTH_CALIBRATION=%s", body)
	return ratchet.CheckExact(name, model, roles, events)
}
