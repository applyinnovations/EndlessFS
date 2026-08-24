package providerbudget

import (
	"errors"
	"fmt"
)

// RatchetLedger is append-only review data. Each new epoch must carry every
// prior operation and may only retain or lower its count, cost, and modeled
// latency ceilings. Removing a pathway or loosening a ceiling fails closed.
type RatchetLedger struct {
	SchemaVersion int            `json:"schemaVersion"`
	Provider      string         `json:"provider"`
	Profile       string         `json:"profile"`
	Epochs        []RatchetEpoch `json:"epochs"`
}

type RatchetEpoch struct {
	ID      string   `json:"id"`
	Budgets []Budget `json:"budgets"`
}

func ParseRatchetLedger(body []byte) (RatchetLedger, error) {
	var ledger RatchetLedger
	if err := decodeStrict(body, &ledger); err != nil {
		return RatchetLedger{}, fmt.Errorf("decode provider budget ratchet: %w", err)
	}
	if ledger.SchemaVersion != 1 || ledger.Provider == "" || ledger.Profile == "" || len(ledger.Epochs) == 0 {
		return RatchetLedger{}, errors.New("provider budget ratchet identity is invalid")
	}
	var prior map[string]Budget
	priorID := ""
	for _, epoch := range ledger.Epochs {
		if epoch.ID == "" || epoch.ID <= priorID || len(epoch.Budgets) == 0 {
			return RatchetLedger{}, errors.New("provider budget ratchet epoch order is invalid")
		}
		current := make(map[string]Budget, len(epoch.Budgets))
		for _, budget := range epoch.Budgets {
			if budget.Name == "" || budget.Provider != ledger.Provider || budget.Profile != ledger.Profile || len(budget.Roles) == 0 {
				return RatchetLedger{}, fmt.Errorf("provider budget ratchet epoch %q contains an invalid budget", epoch.ID)
			}
			if _, exists := current[budget.Name]; exists {
				return RatchetLedger{}, fmt.Errorf("provider budget ratchet epoch %q repeats %q", epoch.ID, budget.Name)
			}
			if err := validateLimits(budget.Maximum); err != nil {
				return RatchetLedger{}, fmt.Errorf("provider budget ratchet %q: %w", budget.Name, err)
			}
			for role, limits := range budget.Roles {
				if !role.valid() {
					return RatchetLedger{}, fmt.Errorf("provider budget ratchet %q has invalid role %q", budget.Name, role)
				}
				if err := validateLimits(limits); err != nil {
					return RatchetLedger{}, fmt.Errorf("provider budget ratchet %q role %q: %w", budget.Name, role, err)
				}
			}
			current[budget.Name] = budget
		}
		if prior != nil {
			for name, previous := range prior {
				next, exists := current[name]
				if !exists {
					return RatchetLedger{}, fmt.Errorf("provider budget ratchet removed pathway %q", name)
				}
				if !limitsTighten(previous.Maximum, next.Maximum) {
					return RatchetLedger{}, fmt.Errorf("provider budget ratchet loosened %q", name)
				}
				for role, previousRole := range previous.Roles {
					nextRole, exists := next.Roles[role]
					if !exists || !limitsTighten(previousRole, nextRole) {
						return RatchetLedger{}, fmt.Errorf("provider budget ratchet loosened %q role %q", name, role)
					}
				}
			}
		}
		prior, priorID = current, epoch.ID
	}
	return ledger, nil
}

func (ledger RatchetLedger) Latest(name string) (Budget, bool) {
	if len(ledger.Epochs) == 0 {
		return Budget{}, false
	}
	for _, budget := range ledger.Epochs[len(ledger.Epochs)-1].Budgets {
		if budget.Name == name {
			return budget, true
		}
	}
	return Budget{}, false
}

func limitsTighten(previous, next Limits) bool {
	return next.Requests <= previous.Requests && next.CostPicoUSD <= previous.CostPicoUSD && next.P50Micros <= previous.P50Micros && next.P95Micros <= previous.P95Micros && next.P99Micros <= previous.P99Micros
}
