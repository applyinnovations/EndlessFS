//go:build !race

package storageformat

// Canonical comparison plus direct Unmarshal measured 23.0 MB/op, versus
// 37.5 MB/op with a full duplicate scan and decoder-owned expanded copies.
const domainPackDecodeAllocationBudget = 26 << 20
const domainPackAllocationMode = "ordinary"

// Exact-wire validation reuse measured 9,915,407–9,915,438 B/op in three
// ordinary runs. Round the observed upper value to the next whole MiB.
const domainPackProvenDecodeAllocationBudget = 10 << 20
