package providerbudget_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

const exactGateModule = "github.com/applyinnovations/endlessfs"
const exactGateSharedHelper = exactGateModule + "/internal/objectstore/budgettest"

type exactGateFile struct {
	path, directory, group string
	imports                map[string]string
	ast                    *ast.File
}

type exactGateFunction struct {
	name, group, location string
	file                  *exactGateFile
	decl                  *ast.FuncDecl
	calls                 []*exactGateFunction
	exact, test           bool
}

func exactGateExempt(fn *exactGateFunction) bool {
	return fn.file.path == "internal/objectstore/budgettest/check.go" && fn.file.ast.Name.Name == "budgettest" && fn.name == "CheckExact" && fn.decl.Recv == nil ||
		fn.file.path == "internal/providerbudget/ratchet_test.go" && fn.file.ast.Name.Name == "providerbudget" && fn.name == "TestRatchetCheckExactReportsMissingCalibrationAndRoleDrift" && fn.test
}

func exactGateIsTest(file *exactGateFile, fn *ast.FuncDecl) bool {
	if !strings.HasSuffix(file.path, "_test.go") || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Results != nil && len(fn.Type.Results.List) != 0 || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	if suffix := strings.TrimPrefix(fn.Name.Name, "Test"); suffix != "" {
		first, _ := utf8.DecodeRuneInString(suffix)
		if unicode.IsLower(first) {
			return false
		}
	}
	parameter := fn.Type.Params.List[0]
	star, ok := parameter.Type.(*ast.StarExpr)
	if !ok || len(parameter.Names) > 1 {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "T" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	return ok && file.imports[qualifier.Name] == "testing"
}

func exactGateCallee(expression ast.Expr) ast.Expr {
	for {
		switch value := expression.(type) {
		case *ast.ParenExpr:
			expression = value.X
		case *ast.IndexExpr:
			expression = value.X
		case *ast.IndexListExpr:
			expression = value.X
		default:
			return expression
		}
	}
}

// Imported aliases are resolved per file; a local variable shadowing an import
// must not acquire the shared helper's explicit exemption.
func exactGateImport(file *exactGateFile, expression ast.Expr) string {
	identifier, ok := expression.(*ast.Ident)
	if !ok || identifier.Obj != nil && identifier.Obj.Kind != ast.Pkg {
		return ""
	}
	return file.imports[identifier.Name]
}

func exactGateSelectedPackage(directory string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimPrefix(root, "./")
		if strings.HasSuffix(root, "/...") {
			root = strings.TrimSuffix(root, "/...")
			if directory == root || strings.HasPrefix(directory, root+"/") {
				return true
			}
		} else if directory == root {
			return true
		}
	}
	return false
}

// Parse both package variants, including test-only helpers. This intentionally
// does not pretend to resolve dynamic function values or method dispatch. Such
// exact-call wrappers require an explicit policy extension instead of omission.
func exactGateInventory(sources map[string][]byte, pattern string, roots []string) ([]string, []string) {
	fset := token.NewFileSet()
	var files []*exactGateFile
	var functions []*exactGateFunction
	byName := make(map[string][]*exactGateFunction)
	byImport := make(map[string][]*exactGateFunction)
	packageNames := make(map[string]map[string]bool)
	var problems []string
	for path, source := range sources {
		parsed, err := parser.ParseFile(fset, path, source, 0)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(path))
		file := &exactGateFile{path: path, directory: directory, group: directory + ":" + parsed.Name.Name, imports: make(map[string]string), ast: parsed}
		for _, imported := range parsed.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			alias := filepath.Base(name)
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			file.imports[alias] = name
		}
		files = append(files, file)
		if !strings.HasSuffix(parsed.Name.Name, "_test") {
			if packageNames[directory] == nil {
				packageNames[directory] = make(map[string]bool)
			}
			packageNames[directory][parsed.Name.Name] = true
		}
		for _, declaration := range parsed.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			function := &exactGateFunction{name: fn.Name.Name, group: file.group, file: file, decl: fn, location: fset.Position(fn.Pos()).String(), test: exactGateIsTest(file, fn)}
			functions = append(functions, function)
			if fn.Recv == nil {
				byName[file.group+":"+function.name] = append(byName[file.group+":"+function.name], function)
				if !strings.HasSuffix(parsed.Name.Name, "_test") {
					key := exactGateModule + "/" + directory + ":" + function.name
					byImport[key] = append(byImport[key], function)
				}
			}
		}
	}
	// An unaliased import uses the declared package name, which need not be
	// its path's final component. Otherwise a cross-package wrapper could be
	// missed when that helper also has a legitimate local Test owner.
	for _, file := range files {
		for _, imported := range file.ast.Imports {
			name, _ := strconv.Unquote(imported.Path.Value)
			if imported.Name != nil || !strings.HasPrefix(name, exactGateModule+"/") {
				continue
			}
			declared := packageNames[strings.TrimPrefix(name, exactGateModule+"/")]
			if len(declared) > 1 {
				problems = append(problems, file.path+": ambiguous imported Go package "+name)
			}
			for alias := range declared {
				delete(file.imports, filepath.Base(name))
				file.imports[alias] = name
			}
		}
	}
	directCalls := make(map[ast.Node]bool)
	ownedExact := make(map[ast.Node]bool)
	for _, fn := range functions {
		ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := exactGateCallee(call.Fun)
			directCalls[callee] = true
			switch target := callee.(type) {
			case *ast.Ident:
				if target.Obj == nil || target.Obj.Kind == ast.Fun {
					fn.calls = append(fn.calls, byName[fn.group+":"+target.Name]...)
					if target.Name == "CheckExact" && len(byName[fn.group+":"+target.Name]) == 0 {
						problems = append(problems, fn.location+": unresolved unqualified CheckExact wrapper")
					}
				}
			case *ast.SelectorExpr:
				imported := exactGateImport(fn.file, target.X)
				if target.Sel.Name == "CheckExact" {
					ownedExact[target] = true
					if imported != "" && imported != exactGateSharedHelper {
						problems = append(problems, fn.location+": unresolved cross-package CheckExact wrapper "+imported)
					}
					fn.exact = true
				} else if imported != "" {
					fn.calls = append(fn.calls, byImport[imported+":"+target.Sel.Name]...)
				}
			}
			return true
		})
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range functions {
			for _, called := range fn.calls {
				if called.exact && !fn.exact {
					fn.exact, changed = true, true
				}
			}
		}
	}
	for _, file := range files {
		var parent ast.Node
		parents := make(map[ast.Node]ast.Node)
		var stack []ast.Node
		ast.Inspect(file.ast, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) != 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
		ast.Inspect(file.ast, func(node ast.Node) bool {
			parent = parents[node]
			switch value := node.(type) {
			case *ast.SelectorExpr:
				if value.Sel.Name == "CheckExact" && !ownedExact[value] {
					problems = append(problems, fset.Position(value.Pos()).String()+": unresolved CheckExact function-value or initializer wrapper")
				}
				imported := exactGateImport(file, value.X)
				for _, target := range byImport[imported+":"+value.Sel.Name] {
					if target.exact && !(imported == exactGateSharedHelper && value.Sel.Name == "CheckExact" && directCalls[value]) {
						problems = append(problems, fset.Position(value.Pos()).String()+": unresolved cross-package exact helper "+imported+"."+value.Sel.Name)
					}
				}
			case *ast.Ident:
				if directCalls[value] || value.Obj != nil && value.Obj.Kind != ast.Fun {
					return true
				}
				if declaration, ok := parent.(*ast.FuncDecl); ok && declaration.Name == value {
					return true
				}
				if selector, ok := parent.(*ast.SelectorExpr); ok && selector.Sel == value {
					return true
				}
				for _, target := range byName[file.group+":"+value.Name] {
					if target.exact {
						problems = append(problems, fset.Position(value.Pos()).String()+": unresolved exact helper function-value "+value.Name)
					}
				}
			}
			return true
		})
	}
	selection, err := regexp.Compile(pattern)
	if err != nil {
		return nil, append(problems, "invalid shared provider-budget test regex: "+err.Error())
	}
	var owners []string
	reachable := make(map[*exactGateFunction]bool)
	var visit func(*exactGateFunction)
	visit = func(fn *exactGateFunction) {
		if reachable[fn] {
			return
		}
		reachable[fn] = true
		for _, called := range fn.calls {
			visit(called)
		}
	}
	for _, fn := range functions {
		if !fn.exact || !fn.test || exactGateExempt(fn) {
			continue
		}
		owners = append(owners, fn.file.directory+":"+fn.name)
		visit(fn)
		if !selection.MatchString(fn.name) {
			problems = append(problems, fn.location+": exact-ratchet test is omitted by shared test regex: "+fn.name)
		}
		if !exactGateSelectedPackage(fn.file.directory, roots) {
			problems = append(problems, fn.location+": exact-ratchet test package is omitted by shared package roots: "+fn.file.directory)
		}
	}
	for _, fn := range functions {
		if !fn.exact || exactGateExempt(fn) {
			continue
		}
		if fn.decl.Recv != nil {
			problems = append(problems, fn.location+": unresolved exact method wrapper "+fn.name)
		} else if !reachable[fn] {
			problems = append(problems, fn.location+": exact helper has no statically resolved application Test owner: "+fn.name)
		}
		if len(byName[fn.group+":"+fn.name]) > 1 {
			problems = append(problems, fn.location+": ambiguous build-variant exact helper "+fn.name)
		}
	}
	sort.Strings(owners)
	sort.Strings(problems)
	return owners, problems
}

func exactGateNixSelection(source string) (string, []string, error) {
	readConstant := func(name string) (string, error) {
		matches := regexp.MustCompile(`(?m)^\s*`+name+`\s*=\s*("[^"\r\n]*")\s*;`).FindAllStringSubmatch(source, -1)
		if len(matches) != 1 || strings.Contains(matches[0][1], "${") {
			return "", fmt.Errorf("%s must be one shared static quoted string", name)
		}
		return strconv.Unquote(matches[0][1])
	}
	pattern, err := readConstant("providerBudgetTestPattern")
	if err != nil {
		return "", nil, err
	}
	packages, err := readConstant("providerBudgetPackages")
	if err != nil {
		return "", nil, err
	}
	for _, root := range strings.Fields(packages) {
		plain := strings.TrimSuffix(strings.TrimPrefix(root, "./"), "/...")
		if !strings.HasPrefix(root, "./") || strings.ContainsAny(root, "'\"$\\") || plain == "" || filepath.ToSlash(filepath.Clean(plain)) != plain || strings.HasPrefix(plain, "../") || plain == ".." {
			return "", nil, fmt.Errorf("unsupported shared provider-budget package root %q", root)
		}
	}
	for _, owner := range []struct{ name, expression string }{
		{"test-provider-budget", `(?s)test-provider-budget\s*=\s*goTask\s*"[^"]+"\s*''(.*?)'';`},
		{"provider-economics", `(?s)providerEconomicsCheck\s*=\s*goCheck\s*"provider-economics"\s*''(.*?)''\s*\[`},
	} {
		matches := regexp.MustCompile(owner.expression).FindAllStringSubmatch(source, -1)
		if len(matches) != 1 {
			return "", nil, fmt.Errorf("cannot resolve unique %s invocation", owner.name)
		}
		found := 0
		for _, line := range strings.Split(strings.ReplaceAll(matches[0][1], "\\\n", " "), "\n") {
			line = strings.Join(strings.Fields(line), " ")
			if line == "go test ${providerBudgetPackages} -run '${providerBudgetTestPattern}' -count=1" {
				found++
			}
		}
		if found != 1 {
			return "", nil, fmt.Errorf("%s must execute shared package roots and regex exactly once", owner.name)
		}
	}
	return pattern, strings.Fields(packages), nil
}

func TestProviderBudgetExactGateInventoryCoversEveryApplicationOwner(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	nix, err := os.ReadFile(filepath.Join(root, "flake.nix"))
	if err != nil {
		t.Fatal(err)
	}
	pattern, packages, err := exactGateNixSelection(string(nix))
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[string][]byte)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "vendor" || entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(relative)], err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	owners, problems := exactGateInventory(sources, pattern, packages)
	for _, problem := range problems {
		t.Error(problem)
	}
	if len(owners) == 0 {
		t.Fatal("application exact-ratchet inventory unexpectedly has no Test owners")
	}
	t.Logf("exact-ratchet inventory: application-owners=%d shared-package-roots=%d", len(owners), len(packages))
}

func TestProviderBudgetExactGateInventoryFailsClosedOnUnsupportedWrappers(t *testing.T) {
	const shared = `package sample
import ("testing"; bt "github.com/applyinnovations/endlessfs/internal/objectstore/budgettest")
func checkInner(t *testing.T) { bt.CheckExact(t) }
func checkOuter(t *testing.T) { checkInner(t) }
`
	for _, fixture := range []struct {
		name, tests, pattern, root, failure string
		extra                               map[string][]byte
	}{
		{name: "transitive-positive", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/sample"},
		{name: "unnamed-parameter-empty-results-positive", tests: `func TestProviderBudgetSelected(*testing.T) () { checkOuter(nil) }`, pattern: "ProviderBudget", root: "./internal/sample"},
		{name: "unnamed-owner-called-as-helper-is-still-selected", tests: `func TestLegacyUnnamed(*testing.T) () { checkOuter(nil) }; func TestProviderBudgetSelected(t *testing.T) { TestLegacyUnnamed(t) }`, pattern: "ProviderBudget", root: "./internal/sample", failure: "omitted by shared test regex: TestLegacyUnnamed"},
		{name: "legacy-not-selected", tests: `func TestLegacyExactOwner(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/sample", failure: "omitted by shared test regex"},
		{name: "package-not-selected", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/other/...", failure: "omitted by shared package roots"},
		{name: "descendant-positive", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/..."},
		{name: "function-value", tests: `func TestProviderBudgetSelected(t *testing.T) { check := checkOuter; check(t) }`, pattern: "ProviderBudget", root: "./internal/sample", failure: "function-value"},
		{name: "orphan-helper", tests: `func TestProviderBudgetNoRatchet(t *testing.T) {}`, pattern: "ProviderBudget", root: "./internal/sample", failure: "no statically resolved application Test owner"},
		{name: "method", tests: `type checker struct{}; func (checker) check(t *testing.T) { checkOuter(t) }; func TestProviderBudgetSelected(t *testing.T) { checker{}.check(t) }`, pattern: "ProviderBudget", root: "./internal/sample", failure: "exact method wrapper"},
		{name: "initializer", tests: `var exact = bt.CheckExact; func TestProviderBudgetSelected(t *testing.T) { exact(t) }`, pattern: "ProviderBudget", root: "./internal/sample", failure: "function-value or initializer"},
		{name: "cross-package", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/...", failure: "cross-package exact helper", extra: map[string][]byte{
			"internal/bridge/bridge.go": []byte(`package bridge
import ("testing"; bt "github.com/applyinnovations/endlessfs/internal/objectstore/budgettest")
func Check(t *testing.T) { bt.CheckExact(t) }`),
			"internal/foreign/foreign_test.go": []byte(`package foreign_test
import ("testing"; bridge "github.com/applyinnovations/endlessfs/internal/bridge")
func TestProviderBudgetForeign(t *testing.T) { bridge.Check(t) }`)}},
		{name: "unknown-checkexact-helper", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/...", failure: "unresolved cross-package CheckExact wrapper", extra: map[string][]byte{
			"internal/foreign/foreign_test.go": []byte(`package foreign_test
import ("testing"; other "example.com/unreviewed")
func TestProviderBudgetForeign(t *testing.T) { other.CheckExact(t) }`)}},
		{name: "declared-import-name-with-local-owner", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/...", failure: "cross-package exact helper", extra: map[string][]byte{
			"internal/misaligned/helper.go": []byte(`package differently
import ("testing"; bt "github.com/applyinnovations/endlessfs/internal/objectstore/budgettest")
func Check(t *testing.T) { bt.CheckExact(t) }`),
			"internal/misaligned/helper_test.go": []byte(`package differently
import "testing"
func TestProviderBudgetLocal(t *testing.T) { Check(t) }`),
			"internal/foreign/foreign_test.go": []byte(`package foreign_test
import ("testing"; "github.com/applyinnovations/endlessfs/internal/misaligned")
func TestProviderBudgetForeign(t *testing.T) { differently.Check(t) }`)}},
		{name: "known-synthetic-positive", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/...", extra: map[string][]byte{
			"internal/providerbudget/ratchet_test.go": []byte(`package providerbudget
import ("testing"; bt "github.com/applyinnovations/endlessfs/internal/objectstore/budgettest")
func TestRatchetCheckExactReportsMissingCalibrationAndRoleDrift(t *testing.T) { bt.CheckExact(t) }`)}},
		{name: "synthetic-exemption-does-not-cover-package", tests: `func TestProviderBudgetSelected(t *testing.T) { checkOuter(t) }`, pattern: "ProviderBudget", root: "./internal/...", failure: "omitted by shared test regex", extra: map[string][]byte{
			"internal/providerbudget/another_test.go": []byte(`package providerbudget
import ("testing"; bt "github.com/applyinnovations/endlessfs/internal/objectstore/budgettest")
func TestRatchetCheckExactReportsMissingCalibrationAndRoleDrift(t *testing.T) { bt.CheckExact(t) }`)}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			sources := map[string][]byte{"internal/sample/sample_test.go": []byte(shared + fixture.tests)}
			for path, source := range fixture.extra {
				sources[path] = source
			}
			owners, problems := exactGateInventory(sources, fixture.pattern, strings.Fields(fixture.root))
			if fixture.failure == "" {
				if len(problems) != 0 || len(owners) != 1 {
					t.Fatalf("valid transitive owner was not selected: owners=%v problems=%v", owners, problems)
				}
			} else if !strings.Contains(strings.Join(problems, "\n"), fixture.failure) {
				t.Fatalf("policy did not reject %s: owners=%v problems=%v", fixture.failure, owners, problems)
			}
		})
	}
}

func TestProviderBudgetExactGateRequiresBothSharedNixInvocations(t *testing.T) {
	const source = `providerBudgetTestPattern = "ProviderBudget";
providerBudgetPackages = "./internal/sample";
test-provider-budget = goTask "budget" ''
go test ${providerBudgetPackages} -run '${providerBudgetTestPattern}' -count=1
'';
providerEconomicsCheck = goCheck "provider-economics" ''
go test ${providerBudgetPackages} -run '${providerBudgetTestPattern}' -count=1
'' [ ];`
	if _, _, err := exactGateNixSelection(source); err != nil {
		t.Fatalf("shared invocation positive control failed: %v", err)
	}
	for _, owner := range []string{"test-provider-budget", "providerEconomicsCheck"} {
		start := strings.Index(source, owner+" =")
		for _, change := range []struct{ old, replacement string }{
			{"go test ${providerBudgetPackages}", "# go test ${providerBudgetPackages}"},
			{"${providerBudgetPackages}", "./internal/sample"},
			{"${providerBudgetTestPattern}", "ProviderBudget"},
		} {
			modified := source[:start] + strings.Replace(source[start:], change.old, change.replacement, 1)
			if _, _, err := exactGateNixSelection(modified); err == nil {
				t.Fatalf("non-shared or commented %s invocation passed policy: %s", owner, change.replacement)
			}
		}
	}
}
