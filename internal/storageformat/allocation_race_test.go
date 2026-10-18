//go:build race

package storageformat

// The same fixture measured 50.9 MB/op with race instrumentation, compared
// with 63.6 MB/op before the canonical-comparison repair. Do not use this instrumented
// allocation budget as a production memory estimate.
const domainPackDecodeAllocationBudget = 56 << 20
const domainPackAllocationMode = "race"

// The same exact-wire reuse fixture measured 17,138,865–17,139,313 B/op in
// three race runs. Round to the next whole MiB; this is not production RSS.
const domainPackProvenDecodeAllocationBudget = 17 << 20
