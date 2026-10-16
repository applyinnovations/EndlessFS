//go:build !race

package storageformat

// Canonical comparison plus direct Unmarshal measured 23.0 MB/op, versus
// 37.5 MB/op with a full duplicate scan and decoder-owned expanded copies.
const domainPackDecodeAllocationBudget = 26 << 20
const domainPackAllocationMode = "ordinary"
