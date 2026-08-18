package cachecontrol

import (
	"net/http"
	"testing"
)

func TestHasNoStoreAcceptsEffectivePolicies(t *testing.T) {
	for name, values := range map[string][]string{
		"exact":                 {"no-store"},
		"case and whitespace":   {" No-Store \t"},
		"additional directives": {"no-cache, no-store, max-age=0"},
		"quoted comma":          {`private="field, value", no-store`},
		"quoted escape":         {`extension="quoted\" value", no-store`},
		"separate fields":       {"private", "max-age=0, NO-STORE"},
	} {
		t.Run(name, func(t *testing.T) {
			header := make(http.Header)
			header["Cache-Control"] = values
			if !HasNoStore(header) {
				t.Fatalf("HasNoStore(%q) = false", values)
			}
		})
	}
}

func TestHasNoStoreRejectsIneffectiveOrMalformedPolicies(t *testing.T) {
	for name, values := range map[string][]string{
		"absent":                 nil,
		"empty":                  {""},
		"other directives":       {"no-cache, max-age=0"},
		"parameterized":          {"no-store=true"},
		"quoted lookalike":       {`private="no-store"`},
		"quoted value lookalike": {`extension="value,no-store"`},
		"leading comma":          {", no-store"},
		"trailing comma":         {"no-store,"},
		"empty directive":        {"private,,no-store"},
		"missing value":          {"max-age=, no-store"},
		"unterminated quote":     {`extension="value, no-store`},
		"dangling quoted escape": {`extension="value\`},
		"escaped control":        {"extension=\"value\\\n\", no-store"},
		"unescaped control":      {"extension=\"value\r\", no-store"},
		"invalid separator":      {"private; no-store"},
		"invalid control":        {"no-store\r"},
		"malformed second field": {"no-store", "="},
	} {
		t.Run(name, func(t *testing.T) {
			header := make(http.Header)
			if values != nil {
				header["Cache-Control"] = values
			}
			if HasNoStore(header) {
				t.Fatalf("HasNoStore(%q) = true", values)
			}
		})
	}
}
