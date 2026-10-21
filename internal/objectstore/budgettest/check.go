package budgettest

import (
	"encoding/json"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

// CheckExact records the complete deterministic candidate vector before
// applying the reviewed ratchet. Missing, changed and unapproved shapes still
// fail closed; calibration never modifies the ledger or supplies headroom.
func CheckExact(t *testing.T, ratchet providerbudget.RatchetLedger, name string, model providerbudget.Model, roles []providerbudget.Role, events []providerbudget.Event) (providerbudget.Report, error) {
	t.Helper()
	budget, err := providerbudget.Calibrate(name, model, roles, events)
	if err != nil {
		return providerbudget.Report{}, err
	}
	body, err := json.Marshal(budget)
	if err != nil {
		return providerbudget.Report{}, err
	}
	t.Logf("PROVIDER_CALIBRATION=%s", body)
	totals, err := model.Estimate(events)
	if err != nil {
		return providerbudget.Report{}, err
	}
	vector, err := json.Marshal(struct {
		Budget providerbudget.Budget `json:"budget"`
		Totals providerbudget.Totals `json:"totals"`
	}{Budget: budget, Totals: totals})
	if err != nil {
		return providerbudget.Report{}, err
	}
	t.Logf("PROVIDER_VECTOR=%s", vector)
	return ratchet.CheckExact(name, model, roles, events)
}
