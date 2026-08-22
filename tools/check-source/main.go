// Command check-source enforces repository constraints that are cheap to
// inspect without external services.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	wallClockFuzzBudget = regexp.MustCompile(`(?:-fuzztime\s+|ENDLESSFS_FUZZTIME:-)[0-9]+(?:ns|us|µs|ms|s|m|h)\b`)
	flakeChecksBlock    = regexp.MustCompile(`(?s)(?:^|\n)\s*checks\s*=\s*forAllSystems.*?(?:^|\n)\s*devShells\s*=`)
)

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

const fileBodyReadGuidance = "reading a stored file body through the Go control plane is forbidden because it consumes provider bandwidth, memory, and CPU in proportion to object size; use provider metadata, server-side copy, or a direct data-plane capability instead"

var fileBodyReadExemptions = map[string]string{
	"internal/preview/service.go": "image-preview-generation",
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
		var content []byte
		if strings.HasSuffix(relative, ".go") {
			content, walkErr = fs.ReadFile(rootFS.FS(), path)
			if walkErr != nil {
				return walkErr
			}
			violations = append(violations, fileBodyViolations(relative, content)...)
		}
		if reason, found := forbiddenNames[name]; found {
			violations = append(violations, fmt.Sprintf("%s: %s", relative, reason))
		}
		if reason, found := forbiddenExtensions[strings.ToLower(filepath.Ext(name))]; found {
			violations = append(violations, fmt.Sprintf("%s: %s", relative, reason))
		}
		if identitySurface(relative) {
			if content == nil {
				content, walkErr = fs.ReadFile(rootFS.FS(), path)
				if walkErr != nil {
					return walkErr
				}
			}
			lower := strings.ToLower(string(content))
			for _, forbidden := range []string{"email", "oauth", "password"} {
				if strings.Contains(lower, forbidden) {
					violations = append(violations, fmt.Sprintf("%s: identity surface contains forbidden %s concept", relative, forbidden))
				}
			}
		}
		if relative == "flake.nix" {
			if content == nil {
				content, walkErr = fs.ReadFile(rootFS.FS(), path)
				if walkErr != nil {
					return walkErr
				}
			}
			if wallClockFuzzBudget.Match(content) {
				violations = append(violations, "flake.nix: fuzz smoke budgets must use an iteration count")
			}
			checks := flakeChecksBlock.Find(content)
			if strings.Contains(string(checks), "ENDLESSFS_RUN_E2E=1") {
				violations = append(violations, "flake.nix: browser runtime gate must run outside the Nix build sandbox")
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

func fileBodyViolations(relative string, content []byte) []string {
	if strings.HasSuffix(relative, "_test.go") || strings.HasPrefix(relative, "vendor/") {
		return nil
	}
	exemption := ""
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		const prefix = "//endlessfs:file-body-read-exemption feature="
		if strings.HasPrefix(line, prefix) {
			exemption = strings.TrimSpace(strings.TrimPrefix(line, prefix))
			break
		}
	}
	if exemption != "" {
		if allowed, ok := fileBodyReadExemptions[relative]; ok && exemption == allowed {
			return nil
		}
		return []string{fmt.Sprintf("%s: file-body-read exemption is not permitted for feature %q", relative, exemption)}
	}
	if strings.HasPrefix(relative, "internal/objectstore/") || strings.HasPrefix(relative, "tools/") {
		return nil
	}

	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, relative, content, parser.ParseComments)
	if err != nil {
		return []string{fmt.Sprintf("%s: parse Go source for file-body policy: %v", relative, err)}
	}
	imports := make(map[string]struct{}, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		value, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		name := path.Base(value)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = struct{}{}
	}

	var violations []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if qualifier, ok := selector.X.(*ast.Ident); ok {
			if _, imported := imports[qualifier.Name]; imported {
				return true
			}
		}
		forbidden := selector.Sel.Name == "Open" && len(call.Args) == 2
		if selector.Sel.Name == "Get" && len(call.Args) == 2 && likelyFileObjectKey(call.Args[1]) {
			forbidden = true
		}
		if !forbidden {
			return true
		}
		position := files.Position(call.Pos())
		violations = append(violations, fmt.Sprintf("%s:%d: %s", relative, position.Line, fileBodyReadGuidance))
		return true
	})
	if strings.Contains(string(content), ".source.CreateDownload(") && strings.Contains(string(content), "response.Body") {
		violations = append(violations, fmt.Sprintf("%s: %s", relative, fileBodyReadGuidance))
	}
	return violations
}

func likelyFileObjectKey(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		name := strings.ToLower(value.Name)
		return strings.Contains(name, "blobkey") || strings.Contains(name, "stagingkey") || strings.Contains(name, "filebodykey")
	case *ast.CallExpr:
		selector, ok := value.Fun.(*ast.SelectorExpr)
		return ok && (selector.Sel.Name == "BlobKey" || selector.Sel.Name == "StagingKey")
	default:
		return false
	}
}

func identitySurface(path string) bool {
	return strings.HasPrefix(path, "internal/model/") ||
		strings.HasPrefix(path, "internal/httpapi/") ||
		strings.HasPrefix(path, "internal/web/ui/")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
