//go:build race

package storageformat

// The same fixture measured 50.9 MB/op with race instrumentation, compared
// with 63.6 MB/op before the canonical-comparison repair. Do not use this instrumented
// allocation budget as a production memory estimate.
const domainPackDecodeAllocationBudget = 56 << 20
const domainPackAllocationMode = "race"
