package providerbudget

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// RatchetDelta is the reviewed on-disk form for one append-only epoch. The
// full in-memory ledger still carries every prior pathway in every epoch; a
// delta merely avoids copying unchanged calibration records into source. Any
// budget omitted by the delta is inherited unchanged. An existing budget may
// only be tightened, never loosened or removed.
type RatchetDelta struct {
	SchemaVersion int      `json:"schemaVersion"`
	Provider      string   `json:"provider"`
	Profile       string   `json:"profile"`
	ID            string   `json:"id"`
	Budgets       []Budget `json:"budgets"`
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

// AppendRatchetDelta strictly decodes and appends one sparse fixture to an
// already validated ledger. The returned epoch is materialized as a complete,
// deterministically ordered snapshot, preserving the original ratchet law for
// all callers.
func AppendRatchetDelta(ledger RatchetLedger, body []byte) (RatchetLedger, error) {
	if len(ledger.Epochs) == 0 {
		return RatchetLedger{}, errors.New("provider budget ratchet delta has no base ledger")
	}
	var delta RatchetDelta
	if err := decodeStrict(body, &delta); err != nil {
		return RatchetLedger{}, fmt.Errorf("decode provider budget ratchet delta: %w", err)
	}
	last := ledger.Epochs[len(ledger.Epochs)-1]
	if delta.SchemaVersion != 1 || delta.Provider != ledger.Provider || delta.Profile != ledger.Profile || delta.ID == "" || delta.ID <= last.ID || len(delta.Budgets) == 0 {
		return RatchetLedger{}, errors.New("provider budget ratchet delta identity is invalid")
	}
	current := make(map[string]Budget, len(last.Budgets)+len(delta.Budgets))
	for _, budget := range last.Budgets {
		current[budget.Name] = budget
	}
	changed := make(map[string]struct{}, len(delta.Budgets))
	for _, budget := range delta.Budgets {
		if _, exists := changed[budget.Name]; exists {
			return RatchetLedger{}, fmt.Errorf("provider budget ratchet delta %q repeats %q", delta.ID, budget.Name)
		}
		changed[budget.Name] = struct{}{}
		if err := validateRatchetBudget(ledger.Provider, ledger.Profile, budget); err != nil {
			return RatchetLedger{}, fmt.Errorf("provider budget ratchet delta %q: %w", delta.ID, err)
		}
		if previous, exists := current[budget.Name]; exists {
			if !limitsTighten(previous.Maximum, budget.Maximum) || !sameRoles(previous.Roles, budget.Roles) {
				return RatchetLedger{}, fmt.Errorf("provider budget ratchet delta loosened %q", budget.Name)
			}
			for role, previousRole := range previous.Roles {
				if !limitsTighten(previousRole, budget.Roles[role]) {
					return RatchetLedger{}, fmt.Errorf("provider budget ratchet delta loosened %q role %q", budget.Name, role)
				}
			}
		}
		current[budget.Name] = budget
	}
	names := make([]string, 0, len(current))
	for name := range current {
		names = append(names, name)
	}
	sort.Strings(names)
	budgets := make([]Budget, len(names))
	for index, name := range names {
		budgets[index] = current[name]
	}
	ledger.Epochs = append(append([]RatchetEpoch(nil), ledger.Epochs...), RatchetEpoch{ID: delta.ID, Budgets: budgets})
	return ledger, nil
}

func validateRatchetBudget(provider, profile string, budget Budget) error {
	if budget.Name == "" || budget.Provider != provider || budget.Profile != profile || len(budget.Roles) == 0 {
		return errors.New("contains an invalid budget")
	}
	if err := validateLimits(budget.Maximum); err != nil {
		return fmt.Errorf("budget %q: %w", budget.Name, err)
	}
	for role, limits := range budget.Roles {
		if !role.valid() {
			return fmt.Errorf("budget %q has invalid role %q", budget.Name, role)
		}
		if err := validateLimits(limits); err != nil {
			return fmt.Errorf("budget %q role %q: %w", budget.Name, role, err)
		}
	}
	return nil
}

func sameRoles(left, right map[Role]Limits) bool {
	if len(left) != len(right) {
		return false
	}
	for role := range left {
		if _, found := right[role]; !found {
			return false
		}
	}
	return true
}

// CheckExact verifies one deterministic workload against the latest ratchet.
// The caller declares every allowed provider role, including roles expected to
// remain at zero. A missing budget returns exact JSON calibration evidence.
func (ledger RatchetLedger) CheckExact(name string, model Model, roles []Role, events []Event) (Report, error) {
	budget, found := ledger.Latest(name)
	if !found {
		calibrated, err := Calibrate(name, model, roles, events)
		if err != nil {
			return Report{}, err
		}
		body, err := json.Marshal(calibrated)
		if err != nil {
			return Report{}, err
		}
		return Report{Budget: calibrated}, fmt.Errorf("provider budget %q is missing; exact calibration=%s", name, body)
	}
	wantRoles := append([]Role(nil), roles...)
	gotRoles := make([]Role, 0, len(budget.Roles))
	for role := range budget.Roles {
		gotRoles = append(gotRoles, role)
	}
	sort.Slice(wantRoles, func(left, right int) bool { return wantRoles[left] < wantRoles[right] })
	sort.Slice(gotRoles, func(left, right int) bool { return gotRoles[left] < gotRoles[right] })
	if len(wantRoles) != len(gotRoles) {
		return Report{}, fmt.Errorf("provider budget %q role contract changed: declared %v, recorded %v", name, wantRoles, gotRoles)
	}
	for index := range wantRoles {
		if wantRoles[index] != gotRoles[index] {
			return Report{}, fmt.Errorf("provider budget %q role contract changed: declared %v, recorded %v", name, wantRoles, gotRoles)
		}
	}
	return budget.CheckRatchet(model, events)
}

func limitsTighten(previous, next Limits) bool {
	criticalTightens := previous.CriticalP50Micros == 0 ||
		next.CriticalP50Micros != 0 && next.CriticalP50Micros <= previous.CriticalP50Micros && next.CriticalP95Micros <= previous.CriticalP95Micros && next.CriticalP99Micros <= previous.CriticalP99Micros
	return next.Requests <= previous.Requests && next.CostPicoUSD <= previous.CostPicoUSD && next.P50Micros <= previous.P50Micros && next.P95Micros <= previous.P95Micros && next.P99Micros <= previous.P99Micros && criticalTightens
}
