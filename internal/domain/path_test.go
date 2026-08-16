package domain

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseUserPathCanonicalizesAndPreservesCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{input: "/", want: "/"},
		{input: "/Documents/Report.txt", want: "/Documents/Report.txt"},
		{input: "/Cafe\u0301/日本語.txt", want: "/Café/日本語.txt"},
		{input: "/Case/case", want: "/Case/case"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			path, err := ParseUserPath(test.input)
			if err != nil {
				t.Fatalf("ParseUserPath() error = %v", err)
			}
			if got := path.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
			if !utf8.ValidString(path.String()) {
				t.Fatal("normalized path is not valid UTF-8")
			}
		})
	}
}

func TestParseUserPathRejectsEveryBoundaryViolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "relative", input: "relative"},
		{name: "repeated separator", input: "/a//b"},
		{name: "trailing separator", input: "/a/"},
		{name: "dot", input: "/a/./b"},
		{name: "dot dot", input: "/a/../b"},
		{name: "backslash", input: `/a\b`},
		{name: "nul", input: "/a\x00b"},
		{name: "ascii control", input: "/a\x1fb"},
		{name: "delete control", input: "/a\x7fb"},
		{name: "reserved metadata", input: "/.ENDLESSFS/private"},
		{name: "reserved trash", input: "/.Trash/item"},
		{name: "segment over byte limit", input: "/" + strings.Repeat("é", 128)},
		{name: "path over byte limit", input: "/" + strings.Repeat("a/", 2048) + "b"},
		{name: "invalid utf8", input: string([]byte{'/', 0xff})},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if path, err := ParseUserPath(test.input); err == nil {
				t.Fatalf("ParseUserPath(%q) = %q, want error", test.input, path.String())
			}
		})
	}
}

func TestUserPathNavigationCannotEscapeRoot(t *testing.T) {
	t.Parallel()

	root := MustParseUserPath("/")
	directory := MustParseUserPath("/Documents")
	file, err := directory.Join("Report.txt")
	if err != nil {
		t.Fatalf("Join() error = %v", err)
	}
	if file.String() != "/Documents/Report.txt" {
		t.Fatalf("Join() = %q", file.String())
	}
	if file.Parent() != directory || directory.Parent() != root || root.Parent() != root {
		t.Fatal("unexpected parent chain")
	}
	if !file.IsDescendantOf(directory) || file.IsDescendantOf(file) || directory.IsDescendantOf(file) {
		t.Fatal("unexpected descendant relationship")
	}
	if _, err := directory.Join("../escape"); err == nil {
		t.Fatal("Join() accepted traversal")
	}
}

func TestUserPathJSONAlwaysRevalidates(t *testing.T) {
	t.Parallel()

	original := MustParseUserPath("/Cafe\u0301/file.txt")
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded UserPath
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded != original {
		t.Fatalf("round trip = %q, want %q", decoded, original)
	}
	if err := json.Unmarshal([]byte(`"/../metadata"`), &decoded); err == nil {
		t.Fatal("Unmarshal() accepted invalid path")
	}
}

func FuzzParseUserPath(f *testing.F) {
	for _, seed := range []string{"/", "/a", "/Cafe\u0301", "/../x", "/.endlessfs/x", "/a//b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		path, err := ParseUserPath(value)
		if err != nil {
			return
		}
		reparsed, err := ParseUserPath(path.String())
		if err != nil || reparsed != path {
			t.Fatalf("canonical path did not reparse: %q, %v", path.String(), err)
		}
	})
}

func FuzzParseUserPathEncodingBoundary(f *testing.F) {
	for _, seed := range []string{
		"/safe/file.txt", "/../escape", "%2F..%2Fescape", "%252F..%252Fescape",
		"/safe%5Cescape", "/.endlessfs/record", "/Cafe%CC%81/file", "%00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		value := encoded
		for range 2 {
			decoded, err := url.PathUnescape(value)
			if err != nil {
				return
			}
			value = decoded
			if parsed, err := ParseUserPath(value); err == nil {
				reparsed, err := ParseUserPath(parsed.String())
				if err != nil || reparsed != parsed {
					t.Fatalf("canonical path did not round-trip: %q -> %#v, %v", value, reparsed, err)
				}
			}
		}
	})
}
