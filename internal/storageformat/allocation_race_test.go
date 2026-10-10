//go:build race

package storageformat

// The same fixture measured 63.6 MB/op with race instrumentation, compared
// with 82.4 MB/op for the original decoder. Do not use this instrumented
// allocation budget as a production memory estimate.
const domainPackDecodeAllocationBudget = 70 << 20
const domainPackAllocationMode = "race"
