package portable

import (
	"sync"
	"testing"
)

// Corrective fixtures retain large staged trees while checking pack encoding
// and recovery. Run independent fixtures concurrently without allowing every
// race-instrumented copy to expand at once. Each fixture still owns its backend,
// clock, IDs, fault hooks and economics ledger.
var correctiveHeavyFixtureSlots = make(chan struct{}, 4)

func acquireCorrectiveHeavyFixture(t *testing.T) func() {
	t.Helper()
	correctiveHeavyFixtureSlots <- struct{}{}
	var released sync.Once
	release := func() { released.Do(func() { <-correctiveHeavyFixtureSlots }) }
	t.Cleanup(release)
	return release
}

func parallelCorrectiveHeavyFixture(t *testing.T) {
	t.Helper()
	t.Parallel()
	acquireCorrectiveHeavyFixture(t)
}
