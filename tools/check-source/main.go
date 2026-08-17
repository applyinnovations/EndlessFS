// Command check-source enforces repository constraints that are cheap to
// inspect without external services.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var wallClockFuzzBudget = regexp.MustCompile(`(?:-fuzztime\s+|ENDLESSFS_FUZZTIME:-)[0-9]+(?:ns|us|µs|ms|s|m|h)\b`)

var forbiddenNames = map[string]string{
	"docker-compose.yml":  "Docker Compose is not a required project tool",
	"docker-compose.yaml": "Docker Compose is not a required project tool",
	"justfile":            "Nix is the only public task interface",
	"makefile":            "Nix is the only public task interface",
	"package.json":        "Node package tooling is prohibited",
	"taskfile.yml":        "Nix is the only public task interface",
	"taskfile.yaml":       "Nix is the only public task interface",
}

var forbiddenExtensions = map[string]string{
	".cs":   ".NET source is prohibited",
	".java": "Java source is prohibited",
	".jsx":  "frontend frameworks are prohibited",
	".php":  "PHP source is prohibited",
	".py":   "Python source is prohibited",
	".rb":   "Ruby source is prohibited",
	".rs":   "Rust source is prohibited",
	".ts":   "TypeScript and Node tooling are prohibited",
	".tsx":  "frontend frameworks are prohibited",
}

func main() {
	root := "."
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: check-source [root]")
		os.Exit(2)
	}
	if len(os.Args) == 2 {
		root = os.Args[1]
	}

	violations, err := check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(violations) != 0 {
		for _, violation := range violations {
			fmt.Fprintln(os.Stderr, violation)
		}
		os.Exit(1)
	}
	fmt.Println("source constraints: ok")
}

func check(root string) ([]string, error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rootFS.Close()

	var violations []string
	err = fs.WalkDir(rootFS.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := strings.ToLower(entry.Name())
		if entry.IsDir() {
			if path != "." && (name == ".git" || name == ".direnv" || name == "vendor" || name == "result" || strings.HasPrefix(name, "result-")) {
				return filepath.SkipDir
			}
			return nil
		}

		relative := filepath.ToSlash(path)
		if reason, found := forbiddenNames[name]; found {
			violations = append(violations, fmt.Sprintf("%s: %s", relative, reason))
		}
		if reason, found := forbiddenExtensions[strings.ToLower(filepath.Ext(name))]; found {
			violations = append(violations, fmt.Sprintf("%s: %s", relative, reason))
		}
		if identitySurface(relative) {
			content, readErr := fs.ReadFile(rootFS.FS(), path)
			if readErr != nil {
				return readErr
			}
			lower := strings.ToLower(string(content))
			for _, forbidden := range []string{"email", "oauth", "password"} {
				if strings.Contains(lower, forbidden) {
					violations = append(violations, fmt.Sprintf("%s: identity surface contains forbidden %s concept", relative, forbidden))
				}
			}
		}
		if relative == "flake.nix" {
			content, readErr := fs.ReadFile(rootFS.FS(), path)
			if readErr != nil {
				return readErr
			}
			if wallClockFuzzBudget.Match(content) {
				violations = append(violations, "flake.nix: fuzz smoke budgets must use an iteration count")
			}
		}

		segments := strings.Split(relative, "/")
		if contains(segments, "themes") {
			extension := strings.ToLower(filepath.Ext(name))
			if extension == ".css" || extension == ".html" || extension == ".js" || extension == ".wasm" || extension == ".tmpl" {
				violations = append(violations, fmt.Sprintf("%s: theme bundles are data-only", relative))
			}
		}
		return nil
	})
	sort.Strings(violations)
	return violations, err
}

func identitySurface(path string) bool {
	return strings.HasPrefix(path, "internal/model/") ||
		strings.HasPrefix(path, "internal/httpapi/") ||
		strings.HasPrefix(path, "internal/web/static/")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
