package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testModified = "2026-09-10T14:48:42Z"

func testFiles() map[string]string {
	return map[string]string{
		"index/db.json":        `{"modified":"` + testModified + `"}`,
		"index/vulns.json":     `[{"id":"GO-2020-0001","modified":"` + testModified + `"}]`,
		"index/modules.json":   `[{"path":"example.com/module","vulns":[{"id":"GO-2020-0001"}]}]`,
		"ID/GO-2020-0001.json": `{"id":"GO-2020-0001","modified":"` + testModified + `","affected":[{"package":{"ecosystem":"Go","name":"example.com/module"}}]}`,
	}
}

func archiveFixture(t *testing.T, files map[string]string, duplicate string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	if duplicate != "" {
		names = append(names, duplicate)
	}
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func retainFixture(t *testing.T, dir string, archive []byte) {
	t.Helper()
	sum := sha256.Sum256(archive)
	m := manifest{Source: sourceURL, SHA256: hex.EncodeToString(sum[:]), RetrievedAt: "2026-09-15T12:00:00Z", DatabaseModified: testModified}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"vulndb.zip": archive, "snapshot.json": body} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetainedSnapshotExtractsWithoutUpstreamOrCache(t *testing.T) {
	dir := t.TempDir()
	files := testFiles()
	retainFixture(t, dir, archiveFixture(t, files, ""))
	output := filepath.Join(t.TempDir(), "database")
	if err := run([]string{"extract", dir, output}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("retained %s: got %q, error %v", name, got, err)
		}
	}
	if err := run([]string{"check", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRejectsChangedOrMissingAuthority(t *testing.T) {
	for _, kind := range []string{"missing-archive", "changed-archive", "missing-manifest", "unknown-manifest-field", "wrong-source", "wrong-modified", "invalid-retrieved", "trailing-manifest"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			retainFixture(t, dir, archiveFixture(t, testFiles(), ""))
			archivePath := filepath.Join(dir, "vulndb.zip")
			manifestPath := filepath.Join(dir, "snapshot.json")
			body, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing-archive":
				err = os.Remove(archivePath)
			case "changed-archive":
				err = os.WriteFile(archivePath, []byte("changed bytes"), 0600)
			case "missing-manifest":
				err = os.Remove(manifestPath)
			case "unknown-manifest-field":
				body = append([]byte(`{"extra":true,`), body[1:]...)
			case "wrong-source":
				body = bytes.ReplaceAll(body, []byte(sourceURL), []byte("https://example.com/vulndb.zip"))
			case "wrong-modified":
				body = bytes.ReplaceAll(body, []byte(testModified), []byte("2026-01-01T00:00:00Z"))
			case "invalid-retrieved":
				body = bytes.ReplaceAll(body, []byte("2026-09-15T12:00:00Z"), []byte("invalid"))
			case "trailing-manifest":
				body = append(body, []byte(` {}`)...)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != "missing-manifest" {
				if err := os.WriteFile(manifestPath, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(t.TempDir(), "must-not-exist")
			if err := run([]string{"extract", dir, output}, io.Discard); err == nil {
				t.Fatal("invalid snapshot was accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("invalid snapshot published output: %v", err)
			}
		})
	}
}

func TestSnapshotRejectsEmptyIncompleteAndUnsafeArchives(t *testing.T) {
	for _, kind := range []string{"empty", "missing-index", "empty-index", "missing-report", "unindexed-report", "unknown-module-report", "bad-json", "bad-report-id", "traversal", "duplicate", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			files := testFiles()
			duplicate := ""
			switch kind {
			case "empty":
				files = map[string]string{}
			case "missing-index":
				delete(files, "index/modules.json")
			case "empty-index":
				files["index/modules.json"] = "[]"
			case "missing-report":
				delete(files, "ID/GO-2020-0001.json")
			case "unindexed-report":
				files["ID/GO-2020-0002.json"] = `{"id":"GO-2020-0002"}`
			case "unknown-module-report":
				files["index/modules.json"] = `[{"path":"example.com/module","vulns":[{"id":"GO-2020-0002"}]}]`
			case "bad-json":
				files["ID/GO-2020-0001.json"] = "{"
			case "bad-report-id":
				files["ID/GO-2020-0001.json"] = `{"id":"GO-2020-0002"}`
			case "traversal":
				files["../escaped.json"] = "{}"
			case "duplicate":
				duplicate = "index/db.json"
			}
			archive := archiveFixture(t, files, duplicate)
			if kind == "truncated" {
				archive = archive[:len(archive)/2]
			}
			dir := t.TempDir()
			retainFixture(t, dir, archive)
			if err := run([]string{"check", dir}, io.Discard); err == nil {
				t.Fatal("invalid archive accepted even though its hash matches")
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateRetainsOfficialBytesAndFailsClosed(t *testing.T) {
	archive := archiveFixture(t, testFiles(), "")
	for _, kind := range []string{"upstream-404", "network-failure", "truncated-response", "empty-database", "valid"} {
		t.Run(kind, func(t *testing.T) {
			good := kind == "valid"
			dir := t.TempDir()
			retainFixture(t, dir, archive)
			before, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != sourceURL || r.Method != http.MethodGet {
					t.Fatalf("unexpected update request: %s %s", r.Method, r.URL)
				}
				status, body := http.StatusOK, archive
				switch kind {
				case "upstream-404":
					status, body = http.StatusNotFound, []byte("No such object")
				case "network-failure":
					return nil, errors.New("network unavailable")
				case "truncated-response":
					body = archive[:len(archive)/2]
				case "empty-database":
					body = archiveFixture(t, map[string]string{}, "")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}
			err = update(dir, client, time.Date(2026, 9, 15, 13, 0, 0, 0, time.UTC))
			if (err == nil) != good {
				t.Fatalf("update error = %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "vulndb.zip"))
			if err != nil || !bytes.Equal(got, archive) {
				t.Fatalf("retained upstream bytes changed: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !good && !bytes.Equal(before, after) {
				t.Fatal("failed update changed the pin")
			}
			if err := run([]string{"check", dir}, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotEnforcesReadBoundsAndRejectsArchiveLinks(t *testing.T) {
	if _, err := readBounded(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("accepted input beyond its memory bound")
	}
	if _, err := readBounded(failingReader{}, 4); err == nil {
		t.Fatal("accepted interrupted input")
	}
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	h := &zip.FileHeader{Name: "index/db.json"}
	h.SetMode(os.ModeSymlink | 0600)
	f, err := w.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "outside"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inspect(body.Bytes()); err == nil {
		t.Fatal("accepted an archive link")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCommandRejectsInvalidUsageAndExistingOutput(t *testing.T) {
	for _, args := range [][]string{nil, {"other", "."}, {"extract", "."}, {"check", ".", "extra"}} {
		if err := run(args, io.Discard); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("arguments %q: %v", args, err)
		}
	}
	dir := t.TempDir()
	retainFixture(t, dir, archiveFixture(t, testFiles(), ""))
	if err := run([]string{"extract", dir, t.TempDir()}, io.Discard); err == nil {
		t.Fatal("extraction accepted an existing output directory")
	}
}
