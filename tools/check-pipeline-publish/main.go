package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// The checker symlinks itself as nix in its private scratch directory. Child
// invocations only record arguments; they never invoke Nix or publish an image.
func init() {
	if filepath.Base(os.Args[0]) != "nix" {
		return
	}
	if err := json.NewEncoder(os.Stdout).Encode(os.Args[1:]); err != nil {
		panic(err)
	}
	os.Exit(0)
}

func main() {
	if len(os.Args) != 2 {
		panic("usage: check-pipeline-publish SCRIPT")
	}
	if err := check(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(script string) error {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	script = strings.ReplaceAll(script, "$(tasks.clone.results.COMMIT_SHA)", revision)
	directory, err := os.MkdirTemp("", "endlessfs-publish-argv-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.Symlink(executable, filepath.Join(directory, "nix")); err != nil {
		return err
	}
	// #nosec G204 G702 -- Execute the reviewed repository CI script using pinned Bash,
	// an isolated environment and a recording-only Nix stub. Flake checks run
	// this development checker inside the non-networked Nix sandbox.
	command := exec.Command("bash", "--noprofile", "--norc", "-e", "-c", script)
	command.Env = []string{"PATH=" + directory + ":" + os.Getenv("PATH")}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("publisher script failed: %w: %s", err, output)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var calls [][]string
	for {
		var arguments []string
		err := decoder.Decode(&arguments)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		calls = append(calls, arguments)
	}
	expected := [][]string{
		{"run", ".#security-freshness"},
		{"run", ".#publish-container", "--", "ghcr.io/applyinnovations/endlessfs:sha-" + revision, "ghcr.io/applyinnovations/endlessfs:edge"},
	}
	if !reflect.DeepEqual(calls, expected) {
		return fmt.Errorf("publisher argument mismatch: got %q; want %q", calls, expected)
	}
	return nil
}
