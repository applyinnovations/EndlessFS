package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type packageInventory struct {
	Package   string
	Roots     []string
	TestFiles map[string]string
}
type goPackage struct {
	ImportPath, Dir           string
	TestGoFiles, XTestGoFiles []string
}

func testName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	tail := strings.TrimPrefix(name, prefix)
	return tail == "" || !unicode.IsLower([]rune(tail)[0])
}
func testSignature(f *ast.FuncType, kind string, aliases map[string]bool) bool {
	if f.TypeParams != nil || f.Results != nil && len(f.Results.List) != 0 || f.Params == nil || len(f.Params.List) != 1 {
		return false
	}
	p := f.Params.List[0]
	if len(p.Names) > 1 {
		return false
	}
	star, ok := p.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if id, ok := star.X.(*ast.Ident); ok {
		return aliases["."] && id.Name == kind
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != kind {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && aliases[id.Name]
}
func within(root, path string) bool {
	p, e := filepath.Rel(root, path)
	return e == nil && p != ".." && !strings.HasPrefix(p, "../") && !filepath.IsAbs(p)
}
func discoverInventory(ctx context.Context, c config, work, source string, env []string, commands map[string]commandResult) []packageInventory {
	commands["inventory"] = runCommand(ctx, c, work, "inventory", c.Go, []string{"list", "-json", "./..."}, env, source, 16<<20)
	if !commandPassed(commands["inventory"]) {
		panic(errContract)
	}
	d := json.NewDecoder(bytes.NewReader(readFile(filepath.Join(work, "private-inventory.stdout"), 16<<20)))
	out := []packageInventory{}
	seen := map[string]bool{}
	for {
		var p goPackage
		e := d.Decode(&p)
		if e == io.EOF {
			break
		}
		must(e)
		if len(out) >= 1000 || !strings.HasPrefix(p.ImportPath, module+"/") || !within(source, p.Dir) || seen[p.ImportPath] {
			panic(errContract)
		}
		seen[p.ImportPath] = true
		spec := packageInventory{Package: p.ImportPath, TestFiles: map[string]string{}}
		names := map[string]bool{}
		parsed := []*ast.File{}
		for _, name := range append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...) {
			if filepath.Base(name) != name || !strings.HasSuffix(name, ".go") {
				panic(errContract)
			}
			file := filepath.Join(p.Dir, name)
			spec.TestFiles[name] = hashFile(file, 16<<20)
			f, e := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments)
			must(e)
			parsed = append(parsed, f)
			aliases := map[string]bool{}
			for _, im := range f.Imports {
				if im.Path.Value == "\"testing\"" {
					alias := "testing"
					if im.Name != nil {
						alias = im.Name.Name
					}
					aliases[alias] = true
				}
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil {
					continue
				}
				kind := ""
				if testName(fn.Name.Name, "Test") {
					kind = "T"
				}
				if testName(fn.Name.Name, "Fuzz") {
					kind = "F"
				}
				if kind != "" && testSignature(fn.Type, kind, aliases) {
					if names[fn.Name.Name] {
						panic(errContract)
					}
					names[fn.Name.Name] = true
				}
			}
		}
		for _, example := range doc.Examples(parsed...) {
			if example.Output != "" || example.EmptyOutput {
				if names["Example"+example.Name] {
					panic(errContract)
				}
				names["Example"+example.Name] = true
			}
		}
		for name := range names {
			spec.Roots = append(spec.Roots, name)
		}
		sort.Strings(spec.Roots)
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	if len(out) == 0 {
		panic(errContract)
	}
	found := false
	for _, p := range out {
		if p.Package == module+"/internal/e2e" {
			for _, name := range p.Roots {
				if name == "TestE2ESmartRecoveryProductionCardinalityWarmTransfersOnlyRemainingFiles" {
					found = true
				}
			}
		}
	}
	if !found {
		panic(errContract)
	}
	return out
}

type observedEvent struct {
	Time                                                   json.RawMessage
	Action, Package, Test, ImportPath, Output, FailedBuild string
	Elapsed                                                float64
}
type safeEvent struct {
	Ordinal                                         uint64
	Package, Root, NodeSHA256, ParentSHA256, Action string
	GoJSONElapsed                                   float64
}
type eventSummary struct {
	Adequate                                                                                          bool
	Packages, Roots, Nodes, AllowedSkips, BuildEvents, OutputEvents, LifecycleRecords, SafeEventBytes uint64
	SafeInventorySHA256                                                                               string
}
type testState struct {
	Parent       string
	Paused, Done bool
}
type packageState struct {
	Done  bool
	Nodes map[string]*testState
}

func allowedSkip(pkg, name string) bool {
	return pkg == module+"/internal/e2e" && name == "TestE2EUploadSourceHardwareQualification" || pkg == module+"/cmd/endlessfs" && name == "TestGenerateCurrentMigrationFixtures"
}
func decodeEvent(line []byte) observedEvent {
	// Accept exactly the selected Go1.26 transport fields; duplicate, null,
	// unknown/miscased keys and unsupported attribute events are inadequate.
	d := json.NewDecoder(bytes.NewReader(line))
	token, e := d.Token()
	must(e)
	if token != json.Delim('{') {
		panic(errContract)
	}
	fields := map[string]json.RawMessage{}
	known := map[string]bool{"Time": true, "Action": true, "Package": true, "Test": true, "ImportPath": true, "Output": true, "Elapsed": true, "FailedBuild": true}
	for d.More() {
		key, e := d.Token()
		must(e)
		name, ok := key.(string)
		if !ok || !known[name] || fields[name] != nil {
			panic(errContract)
		}
		var raw json.RawMessage
		must(d.Decode(&raw))
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			panic(errContract)
		}
		fields[name] = raw
	}
	token, e = d.Token()
	must(e)
	if token != json.Delim('}') {
		panic(errContract)
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		panic(errContract)
	}
	var out observedEvent
	must(json.Unmarshal(line, &out))
	if fields["Action"] == nil || out.Action == "" || math.IsNaN(out.Elapsed) || math.IsInf(out.Elapsed, 0) || out.Elapsed < 0 {
		panic(errContract)
	}
	if (out.Action == "output" || out.Action == "build-output") != (fields["Output"] != nil) {
		panic(errContract)
	}
	if strings.HasPrefix(out.Action, "build-") {
		if fields["ImportPath"] == nil || out.ImportPath == "" || fields["Time"] != nil || fields["Package"] != nil || fields["Test"] != nil || fields["Elapsed"] != nil || fields["FailedBuild"] != nil {
			panic(errContract)
		}
	} else {
		if fields["Time"] == nil || fields["Package"] == nil || out.Package == "" || fields["ImportPath"] != nil {
			panic(errContract)
		}
		var stamp string
		must(json.Unmarshal(out.Time, &stamp))
		_, e := time.Parse(time.RFC3339Nano, stamp)
		must(e)
	}
	return out
}
func validateEvents(path string, inventory []packageInventory, safePath ...string) eventSummary {
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	must(e)
	defer f.Close()
	info, e := f.Stat()
	must(e)
	if info.Size() > maxRaw {
		panic(errContract)
	}
	if len(safePath) > 1 {
		panic(errContract)
	}
	safeHash := sha256.New()
	var sink io.Writer = safeHash
	var safeFile *os.File
	if len(safePath) == 1 {
		safeFile, e = openEvidenceFile(safePath[0], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		must(e)
		defer safeFile.Close()
		sink = io.MultiWriter(safeHash, safeFile)
	}
	want := map[string]map[string]bool{}
	for _, p := range inventory {
		if _, ok := want[p.Package]; ok {
			panic(errContract)
		}
		want[p.Package] = map[string]bool{}
		for _, name := range p.Roots {
			want[p.Package][name] = true
		}
	}
	states := map[string]*packageState{}
	out := eventSummary{}
	var ordinal uint64
	emit := func(e observedEvent, root string) {
		parentSHA := ""
		if state := states[e.Package]; state != nil {
			if node := state.Nodes[e.Test]; node != nil && node.Parent != "" {
				parentSHA = sum([]byte(e.Package + "\x00" + node.Parent))
			}
		}
		record, err := json.Marshal(safeEvent{Ordinal: ordinal, Package: e.Package, Root: root, NodeSHA256: sum([]byte(e.Package + "\x00" + e.Test)), ParentSHA256: parentSHA, Action: e.Action, GoJSONElapsed: e.Elapsed})
		must(err)
		record = append(record, '\n')
		if uint64(len(record)) > maxSafeEvents-out.SafeEventBytes {
			panic(errContract)
		}
		n, err := sink.Write(record)
		must(err)
		out.SafeEventBytes += checkedWriteCount(n, len(record))
		out.LifecycleRecords++
	}
	outputTail := map[string]string{}
	buildTail := map[string]string{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), 2<<20)
	for s.Scan() {
		ordinal++
		if ordinal > 2000000 {
			panic(errContract)
		}
		e := decodeEvent(s.Bytes())
		if e.Action == "build-output" {
			if e.Package != "" || e.Test != "" || e.FailedBuild != "" || len(e.ImportPath) > 512 || len(e.Output) == 0 {
				panic(errContract)
			}
			out.BuildEvents++
			if len(buildTail) >= 4096 {
				if _, ok := buildTail[e.ImportPath]; !ok {
					panic(errContract)
				}
			}
			text := buildTail[e.ImportPath] + e.Output
			if len(text) > 2<<20 {
				panic(errContract)
			}
			if cut := strings.LastIndexByte(text, '\n'); cut >= 0 {
				text = text[cut+1:]
			}
			buildTail[e.ImportPath] = text
			continue
		}
		if e.Action == "build-fail" || e.FailedBuild != "" || e.ImportPath != "" {
			panic(errContract)
		}
		roots, ok := want[e.Package]
		if !ok {
			panic(errContract)
		}
		st := states[e.Package]
		if e.Action == "output" {
			out.OutputEvents++
			key := e.Package + "\x00" + e.Test
			if len(outputTail) > 101000 {
				panic(errContract)
			}
			text := outputTail[key] + e.Output
			if strings.Contains(text, "(cached)") {
				panic(errContract)
			}
			if len(text) > 32 {
				text = text[len(text)-32:]
			}
			outputTail[key] = text
		}
		if e.Test == "" {
			switch e.Action {
			case "start":
				if st != nil {
					panic(errContract)
				}
				states[e.Package] = &packageState{Nodes: map[string]*testState{}}
			case "output":
				if st == nil || st.Done {
					panic(errContract)
				}
			case "pass", "skip":
				if st == nil || st.Done || e.Action == "skip" && len(roots) != 0 {
					panic(errContract)
				}
				for root := range roots {
					if n := st.Nodes[root]; n == nil || !n.Done {
						panic(errContract)
					}
				}
				for _, n := range st.Nodes {
					if !n.Done {
						panic(errContract)
					}
				}
				st.Done = true
				out.Packages++
			default:
				panic(errContract)
			}
			if e.Action != "output" {
				emit(e, "")
			}
			continue
		}
		if st == nil || st.Done || len(e.Test) > 4096 {
			panic(errContract)
		}
		root := strings.SplitN(e.Test, "/", 2)[0]
		if !roots[root] {
			panic(errContract)
		}
		node := st.Nodes[e.Test]
		switch e.Action {
		case "run":
			if node != nil || out.Nodes >= 100000 {
				panic(errContract)
			}
			parent := ""
			if e.Test != root {
				for name, p := range st.Nodes {
					if !p.Done && !p.Paused && strings.HasPrefix(e.Test, name+"/") && len(name) > len(parent) {
						parent = name
					}
				}
				if parent == "" {
					panic(errContract)
				}
			}
			st.Nodes[e.Test] = &testState{Parent: parent}
			out.Nodes++
			if e.Test == root {
				out.Roots++
			}
		case "pause":
			if node == nil || node.Done || node.Paused {
				panic(errContract)
			}
			node.Paused = true
		case "cont":
			if node == nil || node.Done || !node.Paused {
				panic(errContract)
			}
			node.Paused = false
		case "output":
			if node == nil || node.Done {
				panic(errContract)
			}
		case "pass", "skip":
			if node == nil || node.Done || node.Paused {
				panic(errContract)
			}
			for _, child := range st.Nodes {
				if child.Parent == e.Test && !child.Done {
					panic(errContract)
				}
			}
			if e.Action == "skip" {
				if !allowedSkip(e.Package, e.Test) {
					panic(errContract)
				}
				out.AllowedSkips++
			}
			node.Done = true
		default:
			panic(errContract)
		}
		if e.Action != "output" {
			emit(e, root)
		}
	}
	must(s.Err())
	for _, tail := range buildTail {
		if tail != "" {
			panic(errContract)
		}
	}
	if len(states) != len(want) || out.AllowedSkips != 2 {
		panic(errContract)
	}
	for _, st := range states {
		if !st.Done {
			panic(errContract)
		}
	}
	if safeFile != nil {
		must(safeFile.Close())
	}
	out.SafeInventorySHA256 = hex.EncodeToString(safeHash.Sum(nil))
	out.Adequate = true
	return out
}
func position(text string) (uint64, uint64) {
	a, b, ok := strings.Cut(text, ".")
	if !ok {
		panic(errContract)
	}
	line, e := strconv.ParseUint(a, 10, 32)
	must(e)
	column, e := strconv.ParseUint(b, 10, 32)
	must(e)
	if line == 0 || column == 0 {
		panic(errContract)
	}
	return line, column
}
func validateProfile(path string, source map[string]string) uint64 {
	body := readFile(path, maxProfile)
	s := bufio.NewScanner(bytes.NewReader(body))
	s.Buffer(make([]byte, 64<<10), 2<<20)
	if !s.Scan() || s.Text() != "mode: atomic" {
		panic(errContract)
	}
	var blocks, total uint64
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) != 3 {
			panic(errContract)
		}
		colon := strings.LastIndex(fields[0], ":")
		if colon < 0 {
			panic(errContract)
		}
		file := strings.TrimPrefix(fields[0][:colon], module+"/")
		if file == fields[0][:colon] || source[file] == "" {
			panic(errContract)
		}
		a, b, ok := strings.Cut(fields[0][colon+1:], ",")
		if !ok {
			panic(errContract)
		}
		al, ac := position(a)
		bl, bc := position(b)
		if bl < al || bl == al && bc < ac {
			panic(errContract)
		}
		n, e := strconv.ParseUint(fields[1], 10, 32)
		must(e)
		_, e = strconv.ParseUint(fields[2], 10, 64)
		must(e)
		if total > ^uint64(0)-n {
			panic(errContract)
		}
		total += n
		blocks++
	}
	must(s.Err())
	if blocks == 0 || total == 0 {
		panic(errContract)
	}
	return uint64(len(body))
}
