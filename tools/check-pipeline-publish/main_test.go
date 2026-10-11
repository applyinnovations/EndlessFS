package main

import "testing"

func TestPublisherArgumentsPreserveDestinationsAndRejectMalformedCalls(t *testing.T) {
	const destinations = `"ghcr.io/applyinnovations/endlessfs:sha-$(tasks.clone.results.COMMIT_SHA)" "ghcr.io/applyinnovations/endlessfs:edge"`
	slash := string(rune(92))
	for _, test := range []struct {
		name   string
		script string
		valid  bool
	}{
		{name: "single-line", script: "nix run .#security-freshness\nnix run .#publish-container -- " + destinations, valid: true},
		{name: "valid-line-continuation", script: "nix run .#security-freshness\nnix run .#publish-container -- " + slash + "\n" + destinations, valid: true},
		{name: "missing-argument-separator", script: "nix run .#security-freshness\nnix run .#publish-container --" + slash + "\n" + destinations},
		{name: "literal-backslash", script: "nix run .#security-freshness\nnix run .#publish-container -- " + destinations + " " + slash + slash},
		{name: "missing-freshness", script: "nix run .#publish-container -- " + destinations},
		{name: "extra-destination", script: "nix run .#security-freshness\nnix run .#publish-container -- " + destinations + ` "ghcr.io/applyinnovations/endlessfs:another"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := check(test.script); (err == nil) != test.valid {
				t.Fatalf("argument check = %v; valid=%t", err, test.valid)
			}
		})
	}
}
