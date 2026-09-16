// Command vulndb retains the official Go vulnerability database for offline
// verification. Only the explicit update command contacts the upstream service.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const sourceURL = "https://vuln.go.dev/vulndb.zip"

// These are memory safety bounds for the updater, not database freshness or
// advisory-count thresholds. Growing beyond them requires an explicit review.
const maxArchiveBytes = 32 << 20
const maxExpandedBytes = 128 << 20

var reportPath = regexp.MustCompile(`^ID/GO-[0-9]{4}-[0-9]{4,}\.json$`)

type manifest struct {
	Source           string `json:"source"`
	SHA256           string `json:"sha256"`
	RetrievedAt      string `json:"retrievedAt"`
	DatabaseModified string `json:"databaseModified"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) < 2 || (args[0] == "extract" && len(args) != 3) || (args[0] != "extract" && len(args) != 2) {
		return errors.New("usage: vulndb check|update DIRECTORY; vulndb extract DIRECTORY OUTPUT")
	}
	switch args[0] {
	case "update":
		client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		if err := update(args[1], client, time.Now().UTC()); err != nil {
			return err
		}
	case "check", "extract":
	default:
		return errors.New("usage: vulndb check|update DIRECTORY; vulndb extract DIRECTORY OUTPUT")
	}
	m, files, err := load(args[1])
	if err != nil {
		return err
	}
	if args[0] == "extract" {
		// Never merge into an existing database: stale reports must not survive a
		// snapshot change. Nix publishes this output only after the command exits.
		parent, err := os.OpenRoot(filepath.Dir(args[2]))
		if err != nil {
			return err
		}
		defer parent.Close()
		name := filepath.Base(args[2])
		if err := parent.Mkdir(name, 0700); err != nil {
			return err
		}
		root, err := parent.OpenRoot(name)
		if err != nil {
			return err
		}
		defer root.Close()
		for name, body := range files {
			if err := root.MkdirAll(filepath.Dir(name), 0700); err != nil {
				return err
			}
			if err := root.WriteFile(filepath.FromSlash(name), body, 0600); err != nil {
				return err
			}
		}
	}
	_, err = fmt.Fprintf(out, "Go vulnerability snapshot: sha256=%s modified=%s reports=%d\n", m.SHA256, m.DatabaseModified, len(files)-3)
	return err
}

func load(dir string) (manifest, map[string][]byte, error) {
	var m manifest
	root, err := os.OpenRoot(dir)
	if err != nil {
		return m, nil, err
	}
	defer root.Close()
	body, err := readFile(root, "snapshot.json", 1<<20)
	if err != nil {
		return m, nil, err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, nil, fmt.Errorf("snapshot manifest: %w", err)
	}
	if d.Decode(new(any)) != io.EOF || m.Source != sourceURL {
		return m, nil, errors.New("snapshot manifest must contain one object with the official source URL")
	}
	if _, err := time.Parse(time.RFC3339, m.RetrievedAt); err != nil {
		return m, nil, errors.New("snapshot retrieval time must be RFC3339")
	}
	archive, err := readFile(root, "vulndb.zip", maxArchiveBytes)
	if err != nil {
		return m, nil, err
	}
	sum := sha256.Sum256(archive)
	if m.SHA256 != hex.EncodeToString(sum[:]) {
		return m, nil, errors.New("vulnerability snapshot SHA-256 mismatch")
	}
	modified, files, err := inspect(archive)
	if err != nil {
		return m, nil, err
	}
	if modified != m.DatabaseModified {
		return m, nil, errors.New("snapshot manifest and database modification times differ")
	}
	return m, files, nil
}

func readFile(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readBounded(f, limit)
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("vulnerability snapshot exceeds the memory safety limit")
	}
	return body, nil
}

func inspect(archive []byte) (string, map[string][]byte, error) {
	z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return "", nil, err
	}
	files := make(map[string][]byte)
	remaining := int64(maxExpandedBytes)
	for _, f := range z.File {
		index := f.Name == "index/db.json" || f.Name == "index/modules.json" || f.Name == "index/vulns.json"
		if (!index && !reportPath.MatchString(f.Name)) || !f.Mode().IsRegular() || files[f.Name] != nil {
			return "", nil, errors.New("snapshot contains an unexpected, duplicate, or nonregular archive entry")
		}
		r, err := f.Open()
		if err != nil {
			return "", nil, err
		}
		body, err := readBounded(r, remaining)
		err = errors.Join(err, r.Close())
		if err != nil {
			return "", nil, err
		}
		remaining -= int64(len(body))
		if !json.Valid(body) {
			return "", nil, errors.New("snapshot contains invalid JSON")
		}
		files[f.Name] = body
	}
	var db struct {
		Modified string `json:"modified"`
	}
	if err := json.Unmarshal(files["index/db.json"], &db); err != nil {
		return "", nil, fmt.Errorf("database index: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, db.Modified); err != nil {
		return "", nil, errors.New("database index must contain an RFC3339 modification time")
	}
	var vulns []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(files["index/vulns.json"], &vulns); err != nil || len(vulns) == 0 {
		return "", nil, errors.New("snapshot must contain a nonempty vulnerability index")
	}
	indexed := make(map[string]bool)
	for _, v := range vulns {
		var report struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(files["ID/"+v.ID+".json"], &report); err != nil || report.ID != v.ID || indexed[v.ID] {
			return "", nil, errors.New("snapshot vulnerability index and reports disagree")
		}
		indexed[v.ID] = true
	}
	if len(indexed)+3 != len(files) {
		return "", nil, errors.New("snapshot contains unindexed reports or missing indices")
	}
	var modules []struct {
		Path  string `json:"path"`
		Vulns []struct {
			ID string `json:"id"`
		} `json:"vulns"`
	}
	if err := json.Unmarshal(files["index/modules.json"], &modules); err != nil || len(modules) == 0 {
		return "", nil, errors.New("snapshot must contain a nonempty module index")
	}
	for _, module := range modules {
		if module.Path == "" || len(module.Vulns) == 0 {
			return "", nil, errors.New("snapshot contains an incomplete module index entry")
		}
		for _, v := range module.Vulns {
			if !indexed[v.ID] {
				return "", nil, errors.New("snapshot module index refers to a missing report")
			}
		}
	}
	return db.Modified, files, nil
}

func update(dir string, client *http.Client, retrieved time.Time) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	response, err := client.Get(sourceURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("official vulnerability database returned HTTP %d", response.StatusCode)
	}
	archive, err := readBounded(response.Body, maxArchiveBytes)
	if err != nil {
		return err
	}
	modified, _, err := inspect(archive)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	m := manifest{Source: sourceURL, SHA256: hex.EncodeToString(sum[:]), RetrievedAt: retrieved.UTC().Format(time.RFC3339), DatabaseModified: modified}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// This edits a checkout, not live service state. Each file is replaced
	// atomically. An interruption between files leaves a hash mismatch, so
	// verification fails closed until the update is retried or reverted.
	if err := replaceFile(root, "vulndb.zip", archive); err != nil {
		return err
	}
	return replaceFile(root, "snapshot.json", append(body, '\n'))
}

func replaceFile(root *os.Root, name string, body []byte) error {
	temporary := ".vulndb-" + rand.Text()
	f, err := root.OpenFile(temporary, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err := f.Write(body); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(temporary, name)
}
