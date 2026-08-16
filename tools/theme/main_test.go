package main

import (
	"strings"
	"testing"
)

func TestConformanceFixtureOwnsAccessibilityResponsiveAndReducedMotionRules(t *testing.T) {
	for _, required := range []string{"focus-visible", "min-height:var(--efs-metric-targetMinimum)", "@media(max-width:320px)", "@media(prefers-reduced-motion:reduce)", "animation-duration:.01ms!important", "Dialogs", "File browser", "Delete permanently?"} {
		if !strings.Contains(fixtureCSS+fixtureTemplate.Tree.Root.String(), required) {
			t.Fatalf("fixture lacks %q", required)
		}
	}
	for _, forbidden := range []string{"http://", "https://", "@import", "javascript:"} {
		if strings.Contains(fixtureCSS, forbidden) {
			t.Fatalf("fixture CSS has forbidden %q", forbidden)
		}
	}
}
