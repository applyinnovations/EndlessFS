package providerbudget

import (
	"errors"
	"fmt"
)

type Limits struct {
	Requests    int64 `json:"requests"`
	CostPicoUSD int64 `json:"costPicoUSD"`
	P50Micros   int64 `json:"p50Micros"`
	P95Micros   int64 `json:"p95Micros"`
	P99Micros   int64 `json:"p99Micros"`
}

type Budget struct {
	Name     string          `json:"name"`
	Provider string          `json:"provider"`
	Profile  string          `json:"profile"`
	Maximum  Limits          `json:"maximum"`
	Roles    map[Role]Limits `json:"roles"`
}

type Report struct {
	Budget Budget
	Totals Totals
}

func (budget Budget) Check(model Model, events []Event) (Report, error) {
	if budget.Name == "" || budget.Provider == "" || budget.Profile == "" || budget.Provider != model.Provider() || budget.Profile != model.Profile() || len(budget.Roles) == 0 {
		return Report{}, errors.New("provider budget identity is invalid")
	}
	if err := validateLimits(budget.Maximum); err != nil {
		return Report{}, fmt.Errorf("provider budget %q maximum: %w", budget.Name, err)
	}
	for role, limit := range budget.Roles {
		if !role.valid() {
			return Report{}, fmt.Errorf("provider budget %q has invalid role %q", budget.Name, role)
		}
		if err := validateLimits(limit); err != nil {
			return Report{}, fmt.Errorf("provider budget %q role %q: %w", budget.Name, role, err)
		}
	}
	for _, event := range events {
		if _, ok := budget.Roles[event.Role]; !ok {
			return Report{}, fmt.Errorf("provider budget %q: %s role is not budgeted", budget.Name, event.Role)
		}
	}
	totals, err := model.Estimate(events)
	if err != nil {
		return Report{}, err
	}
	report := Report{Budget: budget, Totals: totals}
	if err := checkLimits(budget.Name, "", totalsToLimits(totals), budget.Maximum); err != nil {
		return report, err
	}
	for role, limit := range budget.Roles {
		if err := checkLimits(budget.Name, string(role)+" ", roleTotalsToLimits(totals.ByRole[role]), limit); err != nil {
			return report, err
		}
	}
	return report, nil
}

// CheckRatchet verifies both the upper bound and exact calibration. A cheaper
// observed pathway deliberately fails until a new append-only ratchet epoch
// records the tighter ceilings, so improvements cannot leave stale headroom
// that a later regression could consume unnoticed.
func (budget Budget) CheckRatchet(model Model, events []Event) (Report, error) {
	report, err := budget.Check(model, events)
	if err != nil {
		return report, err
	}
	if observed := totalsToLimits(report.Totals); observed != budget.Maximum {
		return report, fmt.Errorf("provider budget %q calibration changed: observed %+v, recorded %+v; append a tighter ratchet epoch", budget.Name, observed, budget.Maximum)
	}
	for role, limit := range budget.Roles {
		if observed := roleTotalsToLimits(report.Totals.ByRole[role]); observed != limit {
			return report, fmt.Errorf("provider budget %q role %q calibration changed: observed %+v, recorded %+v; append a tighter ratchet epoch", budget.Name, role, observed, limit)
		}
	}
	return report, nil
}

func validateLimits(limits Limits) error {
	if limits.Requests < 0 || limits.CostPicoUSD < 0 || limits.P50Micros < 0 || limits.P95Micros < limits.P50Micros || limits.P99Micros < limits.P95Micros {
		return errors.New("limits are invalid")
	}
	return nil
}

func checkLimits(name, label string, observed, maximum Limits) error {
	for _, metric := range []struct {
		name     string
		observed int64
		maximum  int64
	}{
		{name: "request count", observed: observed.Requests, maximum: maximum.Requests},
		{name: "cost", observed: observed.CostPicoUSD, maximum: maximum.CostPicoUSD},
		{name: "p50 latency", observed: observed.P50Micros, maximum: maximum.P50Micros},
		{name: "p95 latency", observed: observed.P95Micros, maximum: maximum.P95Micros},
		{name: "p99 latency", observed: observed.P99Micros, maximum: maximum.P99Micros},
	} {
		if metric.observed > metric.maximum {
			return fmt.Errorf("provider budget %q exceeded %s%s: observed %d, maximum %d", name, label, metric.name, metric.observed, metric.maximum)
		}
	}
	return nil
}

func totalsToLimits(totals Totals) Limits {
	return Limits{Requests: totals.Requests, CostPicoUSD: totals.CostPicoUSD, P50Micros: totals.P50Micros, P95Micros: totals.P95Micros, P99Micros: totals.P99Micros}
}

func roleTotalsToLimits(totals RoleTotals) Limits {
	return Limits(totals)
}
