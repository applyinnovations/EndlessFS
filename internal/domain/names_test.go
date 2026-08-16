package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDisplayNameNormalizesAndTrims(t *testing.T) {
	t.Parallel()

	name, err := ParseDisplayName("  Cafe\u0301 User  ")
	if err != nil {
		t.Fatalf("ParseDisplayName() error = %v", err)
	}
	if got := name.String(); got != "Café User" {
		t.Fatalf("String() = %q", got)
	}
}

func TestDisplayNameAndCredentialLabelLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		parse func(string) error
		value string
	}{
		{name: "empty display", parse: displayNameError, value: "  "},
		{name: "display code points", parse: displayNameError, value: strings.Repeat("a", 101)},
		{name: "display bytes", parse: displayNameError, value: strings.Repeat("界", 86)},
		{name: "empty label", parse: credentialLabelError, value: ""},
		{name: "label code points", parse: credentialLabelError, value: strings.Repeat("a", 65)},
		{name: "nul", parse: displayNameError, value: "a\x00b"},
		{name: "control", parse: displayNameError, value: "a\tb"},
		{name: "line separator", parse: displayNameError, value: "a\u2028b"},
		{name: "paragraph separator", parse: displayNameError, value: "a\u2029b"},
		{name: "invalid utf8", parse: displayNameError, value: string([]byte{0xff})},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.parse(test.value); err == nil {
				t.Fatalf("accepted %q", test.value)
			}
		})
	}
}

func TestCredentialLabelUsesShorterLimit(t *testing.T) {
	t.Parallel()

	label, err := ParseCredentialLabel(strings.Repeat("🔑", 64))
	if err != nil {
		t.Fatalf("ParseCredentialLabel() error = %v", err)
	}
	if utf8.RuneCountInString(label.String()) != 64 {
		t.Fatalf("label = %q", label)
	}
}

func displayNameError(value string) error {
	_, err := ParseDisplayName(value)
	return err
}

func credentialLabelError(value string) error {
	_, err := ParseCredentialLabel(value)
	return err
}
