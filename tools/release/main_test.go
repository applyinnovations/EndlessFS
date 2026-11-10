package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCheckTag(t *testing.T) {
	for _, tag := range []string{"v0.0.0", "v1.2.3", "v10.20.30"} {
		if err := checkTag(tag); err != nil {
			t.Errorf("checkTag(%q) = %v, want nil", tag, err)
		}
	}
	for _, tag := range []string{
		"", "1.2.3", "v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.03",
		"v1.2.3-rc1", "v1.2.3\n", "v1.2.3;id", "v1.2.$(id)", "refs/tags/v1.2.3", "V1.2.3",
	} {
		if err := checkTag(tag); err == nil {
			t.Errorf("checkTag(%q) = nil, want an error", tag)
		}
	}
}

type fakeGH struct {
	calls [][]string
	view  func() (string, error)
}

func (f *fakeGH) run(arguments []string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), arguments...))
	if arguments[1] == "view" {
		return f.view()
	}
	return "", nil
}

var assets = []string{"result-release/SHA256SUMS", "result-release/endlessfs-linux-amd64"}

func TestPublishUploadsToAnExistingRelease(t *testing.T) {
	gh := &fakeGH{view: func() (string, error) { return "", nil }}
	if err := publish("v1.2.3", "applyinnovations/EndlessFS", assets, gh.run); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"release", "view", "v1.2.3", "--repo", "applyinnovations/EndlessFS"},
		{"release", "upload", "v1.2.3", "--repo", "applyinnovations/EndlessFS", "--clobber", assets[0], assets[1]},
	}
	if !reflect.DeepEqual(gh.calls, want) {
		t.Fatalf("calls = %q, want %q", gh.calls, want)
	}
}

func TestPublishCreatesAMissingRelease(t *testing.T) {
	gh := &fakeGH{view: func() (string, error) { return "release not found\n", errors.New("exit status 1") }}
	if err := publish("v1.2.3", "applyinnovations/EndlessFS", assets, gh.run); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"release", "view", "v1.2.3", "--repo", "applyinnovations/EndlessFS"},
		{"release", "create", "v1.2.3", "--repo", "applyinnovations/EndlessFS", "--verify-tag", "--generate-notes", "--title", "v1.2.3", assets[0], assets[1]},
	}
	if !reflect.DeepEqual(gh.calls, want) {
		t.Fatalf("calls = %q, want %q", gh.calls, want)
	}
}

// A failed lookup that is not "release not found" (an expired token, a
// network error) must not be mistaken for a missing release.
func TestPublishStopsOnOtherViewFailures(t *testing.T) {
	gh := &fakeGH{view: func() (string, error) { return "HTTP 401: Bad credentials\n", errors.New("exit status 1") }}
	err := publish("v1.2.3", "applyinnovations/EndlessFS", assets, gh.run)
	if err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("publish = %v, want the view failure", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("calls = %q, want only the view", gh.calls)
	}
}

func TestPublishRejectsBadInputsBeforeCallingGH(t *testing.T) {
	for _, c := range []struct{ tag, repository string }{
		{"v1.2", "applyinnovations/EndlessFS"},
		{"v1.2.3", "EndlessFS"},
		{"v1.2.3", "--repo=x/y z"},
	} {
		gh := &fakeGH{view: func() (string, error) { return "", nil }}
		if err := publish(c.tag, c.repository, assets, gh.run); err == nil {
			t.Errorf("publish(%q, %q) = nil, want an error", c.tag, c.repository)
		}
		if len(gh.calls) != 0 {
			t.Errorf("publish(%q, %q) called gh: %q", c.tag, c.repository, gh.calls)
		}
	}
}

func TestReleaseAssets(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"b.tar.gz", "SHA256SUMS", "a.json"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "result-release")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	got, err := releaseAssets(link)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(link, "SHA256SUMS"), filepath.Join(link, "a.json"), filepath.Join(link, "b.tar.gz")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("releaseAssets = %q, want %q", got, want)
	}

	if _, err := releaseAssets(t.TempDir()); err == nil {
		t.Error("an empty directory must not publish a release without assets")
	}
	nested := t.TempDir()
	if err := os.Mkdir(filepath.Join(nested, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseAssets(nested); err == nil {
		t.Error("a subdirectory must be rejected, not skipped")
	}
}

func TestRunUsage(t *testing.T) {
	never := func([]string) (string, error) { t.Fatal("gh called"); return "", nil }
	for _, arguments := range [][]string{nil, {"check-tag"}, {"publish", "v1.2.3"}, {"delete", "v1.2.3"}} {
		if err := run(arguments, never); err == nil {
			t.Errorf("run(%q) = nil, want a usage error", arguments)
		}
	}
	if err := run([]string{"check-tag", "v1.2.3"}, never); err != nil {
		t.Errorf("run(check-tag v1.2.3) = %v", err)
	}
}
