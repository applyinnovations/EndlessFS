// Command release holds the release pipeline's decisions so that the Tekton
// definition only calls the release-verify and publish-release flake apps.
//
//	release check-tag TAG
//	release publish TAG OWNER/REPO DIRECTORY
//
// check-tag accepts only vMAJOR.MINOR.PATCH without leading zeros. publish
// uploads every regular file in DIRECTORY to the GitHub release for TAG with
// gh, creating the release when GitHub reports that it does not exist yet and
// replacing same-named assets when it does, so a retried run converges.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// errNoRelease is how gh reports a missing release on stderr. Any other
// failure of "gh release view" stops the run instead of creating a release.
const errNoRelease = "release not found"

// runner runs gh with arguments and returns its standard error.
type runner func(arguments []string) (string, error)

func main() {
	if err := run(os.Args[1:], ghRunner(os.Stdout, os.Stderr)); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(2)
	}
}

func run(arguments []string, gh runner) error {
	if len(arguments) == 0 {
		return errors.New("usage: release check-tag TAG | release publish TAG OWNER/REPO DIRECTORY")
	}
	switch arguments[0] {
	case "check-tag":
		if len(arguments) != 2 {
			return errors.New("usage: release check-tag TAG")
		}
		return checkTag(arguments[1])
	case "publish":
		if len(arguments) != 4 {
			return errors.New("usage: release publish TAG OWNER/REPO DIRECTORY")
		}
		assets, err := releaseAssets(arguments[3])
		if err != nil {
			return err
		}
		return publish(arguments[1], arguments[2], assets, gh)
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func checkTag(tag string) error {
	if !tagPattern.MatchString(tag) {
		return fmt.Errorf("release checkout %q is not a vMAJOR.MINOR.PATCH tag", tag)
	}
	return nil
}

// releaseAssets lists the regular files directly inside directory, sorted.
// Symbolic links are followed so a Nix result link works as the directory.
func releaseAssets(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var assets []string
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		// #nosec G703 -- Reads only entries listed in the release directory
		// the flake app passes (its own nix build output).
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("release asset %s is not a regular file", path)
		}
		assets = append(assets, path)
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("no release assets in %s", directory)
	}
	sort.Strings(assets)
	return assets, nil
}

func publish(tag, repository string, assets []string, gh runner) error {
	if err := checkTag(tag); err != nil {
		return err
	}
	if !repositoryPattern.MatchString(repository) {
		return fmt.Errorf("repository %q is not OWNER/REPO", repository)
	}
	stderr, err := gh([]string{"release", "view", tag, "--repo", repository})
	switch {
	case err == nil:
		_, err = gh(append([]string{"release", "upload", tag, "--repo", repository, "--clobber"}, assets...))
		return err
	case strings.Contains(stderr, errNoRelease):
		arguments := []string{"release", "create", tag, "--repo", repository, "--verify-tag", "--generate-notes", "--title", tag}
		_, err = gh(append(arguments, assets...))
		return err
	default:
		return fmt.Errorf("gh release view %s: %w: %s", tag, err, strings.TrimSpace(stderr))
	}
}

func ghRunner(stdout, stderr io.Writer) runner {
	return func(arguments []string) (string, error) {
		var captured bytes.Buffer
		// #nosec G204 G702 -- gh comes from the flake app's pinned PATH; the
		// arguments are a validated tag, repository and asset paths.
		command := exec.Command("gh", arguments...)
		command.Stdout = stdout
		command.Stderr = io.MultiWriter(stderr, &captured)
		err := command.Run()
		return captured.String(), err
	}
}
