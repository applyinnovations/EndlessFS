package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Handmade from the pinned Go writer's documented 175-byte wire layout; no
// production normalizer/parser constructs the expected bytes for this oracle.
func goldenAction() (string, string, []byte, []byte) {
	action := strings.Repeat("0123456789abcdef", 4)
	output := strings.Repeat("fedcba9876543210", 4)
	prefix := "v1 " + action + " " + output + " " + "                 123 "
	input := []byte(prefix + " 1234567890123456789\n")
	expected := []byte(prefix + "                   0\n")
	return action + "-a", output + "-d", input, expected
}

// Canonicalize only test-owned scratch created by testing, including Darwin's
// /var alias. The installer must never resolve an arbitrary destination link.
func ownedTestDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNormalizedActionGoldenAndIdempotence(t *testing.T) {
	name, _, input, expected := goldenAction()
	untouched := append([]byte(nil), input...)
	if len(input) != 175 || len(expected) != 175 {
		t.Fatal("handmade pinned-format fixture is invalid")
	}
	actual, changed, err := normalizedAction(name, input)
	if err != nil || !changed || !bytes.Equal(actual, expected) || !bytes.Equal(input, untouched) {
		t.Fatal("normalization changed protected fields/input or failed the independent golden")
	}
	again, changed, err := normalizedAction(name, actual)
	if err != nil || changed || !bytes.Equal(again, expected) {
		t.Fatal("normalization is not byte-idempotent")
	}
}

func TestNormalizedActionRejectsMalformedPinnedMetadata(t *testing.T) {
	name, _, golden, _ := goldenAction()
	for _, fixture := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"short", func(body []byte) []byte { return body[:174] }},
		{"long", func(body []byte) []byte { return append(body, '\n') }},
		{"version", func(body []byte) []byte { body[1] = '2'; return body }},
		{"separator", func(body []byte) []byte { body[67] = ':'; return body }},
		{"action_binding", func(body []byte) []byte { body[3] = 'f'; return body }},
		{"output_hex", func(body []byte) []byte { body[68] = 'z'; return body }},
		{"uppercase_output", func(body []byte) []byte { body[68] = 'F'; return body }},
		{"negative_size", func(body []byte) []byte { copy(body[133:153], fmt.Sprintf("%20d", -1)); return body }},
		{"size_overflow", func(body []byte) []byte { copy(body[133:153], strings.Repeat("9", 20)); return body }},
		{"blank_size", func(body []byte) []byte { copy(body[133:153], strings.Repeat(" ", 20)); return body }},
		{"noncanonical_size", func(body []byte) []byte { copy(body[133:153], "00000000000000000123"); return body }},
		{"negative_time", func(body []byte) []byte { copy(body[154:174], fmt.Sprintf("%20d", -1)); return body }},
		{"time_overflow", func(body []byte) []byte { copy(body[154:174], strings.Repeat("9", 20)); return body }},
		{"blank_time", func(body []byte) []byte { copy(body[154:174], strings.Repeat(" ", 20)); return body }},
		{"time_text", func(body []byte) []byte { body[173] = 'x'; return body }},
		{"noncanonical_time", func(body []byte) []byte { copy(body[154:174], "00000000000000000000"); return body }},
		{"newline", func(body []byte) []byte { body[174] = ' '; return body }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			body := fixture.mutate(append([]byte(nil), golden...))
			before := append([]byte(nil), body...)
			if _, _, err := normalizedAction(name, body); !errors.Is(err, errAction) || !bytes.Equal(body, before) {
				t.Fatal("malformed action was accepted or its input was mutated")
			}
		})
	}
	for _, invalid := range []string{"", name + "x", strings.ToUpper(name), name[:64] + "-d"} {
		if _, _, err := normalizedAction(invalid, golden); !errors.Is(err, errAction) {
			t.Fatal("malformed action filename was accepted")
		}
	}
}

func TestNormalizedActionAcceptsExactDecimalBounds(t *testing.T) {
	name, _, golden, _ := goldenAction()
	for _, field := range []string{"                   0", " 9223372036854775807"} {
		body := append([]byte(nil), golden...)
		copy(body[133:153], field)
		copy(body[154:174], field)
		actual, _, err := normalizedAction(name, body)
		if err != nil || string(actual[133:153]) != field || string(actual[154:174]) != "                   0" {
			t.Fatal("pinned canonical nonnegative int64 boundary was rejected or size was changed")
		}
	}
}

type syntheticOwnershipInfo struct {
	os.FileInfo
	uid uint32
}

func (info syntheticOwnershipInfo) Sys() any {
	return &syscall.Stat_t{Uid: info.uid}
}

func TestCacheDirectoryOwnershipIsRequiredWithoutChangingHostOwnership(t *testing.T) {
	info, err := os.Stat(ownedTestDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	owner := uint32(os.Getuid())
	if err := ownedDirectoryInfo(syntheticOwnershipInfo{FileInfo: info, uid: owner}); err != nil {
		t.Fatal("valid owner was rejected")
	}
	if err := ownedDirectoryInfo(syntheticOwnershipInfo{FileInfo: info, uid: owner ^ 1}); !errors.Is(err, errOwner) {
		t.Fatal("foreign owner was accepted")
	}
}

func seedFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := ownedTestDirectory(t)
	action, data, body, _ := goldenAction()
	for _, shard := range []string{action[:2], data[:2]} {
		if err := os.Mkdir(filepath.Join(root, shard), 0700); err != nil {
			t.Fatal(err)
		}
	}
	actionPath := filepath.Join(root, action[:2], action)
	dataPath := filepath.Join(root, data[:2], data)
	for path, contents := range map[string][]byte{
		actionPath:                      body,
		dataPath:                        []byte("UNCHANGED_COMPILER_ARTIFACT"),
		filepath.Join(root, "README"):   []byte("known cache metadata"),
		filepath.Join(root, "trim.txt"): []byte("1234567890"),
	} {
		if err := os.WriteFile(path, contents, 0400); err != nil {
			t.Fatal(err)
		}
	}
	return root, actionPath, dataPath
}

func TestNormalizeTreePreservesArtifactsModesAndIsIdempotent(t *testing.T) {
	root, actionPath, dataPath := seedFixture(t)
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := normalize(root)
	if err != nil || result.Actions != 1 || result.Normalized != 1 || !result.TrimRemoved {
		t.Fatal("normalization did not cover the actual action/trim files")
	}
	_, _, _, expected := goldenAction()
	action, actionErr := os.ReadFile(actionPath)
	data, dataErr := os.ReadFile(dataPath)
	info, statErr := os.Stat(actionPath)
	if actionErr != nil || dataErr != nil || statErr != nil || !bytes.Equal(action, expected) || !bytes.Equal(data, before) || info.Mode().Perm() != 0400 {
		t.Fatal("normalization changed compiled bytes or action permissions")
	}
	result, err = normalize(root)
	if err != nil || result.Actions != 1 || result.Normalized != 0 || result.TrimRemoved {
		t.Fatal("tree normalization is not idempotent")
	}
}

func TestInstallPreservesWarmEntriesAndOwnsWritableIndependentCopies(t *testing.T) {
	source, actionPath, dataPath := seedFixture(t)
	if _, err := normalize(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataPath, 0500); err != nil {
		t.Fatal(err)
	}
	destination := ownedTestDirectory(t)
	result, err := install(source, destination)
	if err != nil || result.Copied != 3 || result.Existing != 0 {
		t.Fatal("fresh seed did not copy every expected file")
	}
	for _, sourcePath := range []string{actionPath, dataPath} {
		relative, _ := filepath.Rel(source, sourcePath)
		targetPath := filepath.Join(destination, relative)
		sourceBytes, sourceErr := os.ReadFile(sourcePath)
		targetBytes, targetErr := os.ReadFile(targetPath)
		sourceInfo, sourceStatErr := os.Stat(sourcePath)
		targetInfo, targetStatErr := os.Stat(targetPath)
		if sourceErr != nil || targetErr != nil || sourceStatErr != nil || targetStatErr != nil || !bytes.Equal(sourceBytes, targetBytes) || targetInfo.Mode().Perm()&0200 == 0 || targetInfo.Mode().Perm()&0100 != sourceInfo.Mode().Perm()&0100 {
			t.Fatal("seed copy lost bytes, writability or executable mode")
		}
		a, aOK := sourceInfo.Sys().(*syscall.Stat_t)
		b, bOK := targetInfo.Sys().(*syscall.Stat_t)
		if !aOK || !bOK || a.Dev == b.Dev && a.Ino == b.Ino || b.Nlink != 1 {
			t.Fatal("seed installation reused an inode/hardlink")
		}
	}
	_, dataName, _, _ := goldenAction()
	warmPath := filepath.Join(destination, dataName[:2], dataName)
	warm := []byte("PRESERVE_WARM_ENTRY")
	if err := os.WriteFile(warmPath, warm, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(warmPath, 0400); err != nil {
		t.Fatal(err)
	}
	result, err = install(source, destination)
	actual, readErr := os.ReadFile(warmPath)
	info, statErr := os.Stat(warmPath)
	if err != nil || readErr != nil || statErr != nil || result.Copied != 0 || result.Existing != 3 || !bytes.Equal(actual, warm) || info.Mode().Perm() != 0400 {
		t.Fatal("repeated overlay clobbered an existing cache file or its mode")
	}
	// Simulate Go trimming one ordinary entry. Reinstallation has no stamp
	// shortcut: it must restore the missing entry while preserving warm files.
	if err := os.Remove(filepath.Join(destination, filepath.Base(filepath.Dir(actionPath)), filepath.Base(actionPath))); err != nil {
		t.Fatal(err)
	}
	result, err = install(source, destination)
	if err != nil || result.Copied != 1 || result.Existing != 2 {
		t.Fatal("trimmed seed entries were not restored independently")
	}
}

func TestCacheSeedRejectsSourceAndDestinationSymlinksWithoutOutsideWrites(t *testing.T) {
	for _, fixture := range []string{"source", "destination_ancestor", "destination_root", "destination_shard", "destination_file"} {
		t.Run(fixture, func(t *testing.T) {
			source, actionPath, _ := seedFixture(t)
			if _, err := normalize(source); err != nil {
				t.Fatal(err)
			}
			destination, outside := ownedTestDirectory(t), ownedTestDirectory(t)
			outsideFile := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(outsideFile, []byte("PRIVATE_SENTINEL"), 0600); err != nil {
				t.Fatal(err)
			}
			var link, target string
			switch fixture {
			case "source":
				link, target = filepath.Join(source, "aa"), outside
			case "destination_root":
				link, target = filepath.Join(destination, "linked"), outside
				destination = link
			case "destination_ancestor":
				link, target = filepath.Join(destination, "linked"), outside
				destination = filepath.Join(link, "not-created", "cache")
			case "destination_shard":
				link, target = filepath.Join(destination, filepath.Base(filepath.Dir(actionPath))), outside
			case "destination_file":
				shard := filepath.Join(destination, filepath.Base(filepath.Dir(actionPath)))
				if err := os.Mkdir(shard, 0700); err != nil {
					t.Fatal(err)
				}
				link, target = filepath.Join(shard, filepath.Base(actionPath)), outsideFile
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, err := install(source, destination); !errors.Is(err, errTree) {
				t.Fatal("symlink cache path was accepted")
			}
			actual, err := os.ReadFile(outsideFile)
			entries, listErr := os.ReadDir(outside)
			if err != nil || listErr != nil || string(actual) != "PRIVATE_SENTINEL" || len(entries) != 1 {
				t.Fatal("cache helper wrote through a symlink")
			}
		})
	}
}

func TestInstallPreservesExecutableDataDirectoryLayout(t *testing.T) {
	source := ownedTestDirectory(t)
	name := strings.Repeat("ab", 32) + "-d"
	directory := filepath.Join(source, name[:2], name)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cache-seed"), []byte("EXECUTABLE_ARTIFACT"), 0500); err != nil {
		t.Fatal(err)
	}
	destination := ownedTestDirectory(t)
	result, err := install(source, destination)
	actual, readErr := os.ReadFile(filepath.Join(destination, name[:2], name, "cache-seed"))
	info, statErr := os.Stat(filepath.Join(destination, name[:2], name, "cache-seed"))
	if err != nil || readErr != nil || statErr != nil || result.Copied != 1 || string(actual) != "EXECUTABLE_ARTIFACT" || info.Mode().Perm()&0300 != 0300 {
		t.Fatal("pinned executable-cache directory layout or modes were lost")
	}
}

type gatedCopyReader struct {
	payload []byte
	offset  int
	paused  bool
	ready   chan struct{}
	resume  <-chan struct{}
}

func (reader *gatedCopyReader) Read(destination []byte) (int, error) {
	if reader.offset == len(reader.payload) {
		return 0, io.EOF
	}
	if reader.offset > 0 && !reader.paused {
		reader.paused = true
		close(reader.ready)
		<-reader.resume
	}
	remaining := reader.payload[reader.offset:]
	if reader.offset == 0 {
		remaining = remaining[:len(remaining)/2]
	}
	count := copy(destination, remaining)
	reader.offset += count
	return count, nil
}

type publicationResult struct {
	copied bool
	bytes  uint64
	err    error
}

func TestPublishMissingKeepsPartialPrivateAndConcurrentWinnerComplete(t *testing.T) {
	directory := ownedTestDirectory(t)
	target := filepath.Join(directory, strings.Repeat("ab", 32)+"-d")
	payload := bytes.Repeat([]byte("COMPLETE_COMPILER_ARTIFACT"), 4096)
	ready, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	t.Cleanup(release)
	first := make(chan publicationResult, 1)
	go func() {
		copied, count, err := publishMissing(&gatedCopyReader{payload: payload, ready: ready, resume: resume}, int64(len(payload)), 0500, target)
		first <- publicationResult{copied, count, err}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("first copy did not reach the controlled partial-file boundary")
	}
	// A separate reader is active while the first writer owns a partial staged
	// file. The valid cache name must remain absent, not expose a short prefix.
	type readResult struct {
		body []byte
		err  error
	}
	readNow, reads := make(chan struct{}), make(chan readResult)
	go func() {
		for range readNow {
			body, err := os.ReadFile(target)
			reads <- readResult{body, err}
		}
	}()
	t.Cleanup(func() { close(readNow) })
	readNow <- struct{}{}
	before := <-reads
	if !errors.Is(before.err, os.ErrNotExist) || len(before.body) != 0 {
		t.Fatal("a cache reader observed a partially copied published filename")
	}
	// A competing installer can finish first. Atomic no-replace publication
	// lets exactly this complete private inode win and preserves its mode.
	second := make(chan publicationResult, 1)
	go func() {
		copied, count, err := publishMissing(bytes.NewReader(payload), int64(len(payload)), 0400, target)
		second <- publicationResult{copied, count, err}
	}()
	select {
	case result := <-second:
		if result.err != nil || !result.copied || result.bytes != uint64(len(payload)) {
			t.Fatal("complete competing copy did not publish independently")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("competing complete copy was blocked behind an unrelated partial copy")
	}
	readNow <- struct{}{}
	after := <-reads
	if after.err != nil || !bytes.Equal(after.body, payload) {
		t.Fatal("concurrent reader did not observe the entire winning artifact")
	}
	release()
	select {
	case result := <-first:
		if result.err != nil || result.copied || result.bytes != 0 {
			t.Fatal("late copy replaced or misreported the existing complete winner")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late copy did not finish after its controlled release")
	}
	readNow <- struct{}{}
	final := <-reads
	info, statErr := os.Stat(target)
	entries, listErr := os.ReadDir(directory)
	if final.err != nil || !bytes.Equal(final.body, payload) || statErr != nil || info.Mode().Perm() != 0600 || listErr != nil || len(entries) != 1 {
		t.Fatal("late copy changed winning contents/mode or left a staging file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		t.Fatal("completed publication retained a second staging hardlink")
	}
}

type interruptedCopyReader struct{}

func (interruptedCopyReader) Read([]byte) (int, error) {
	return 0, errors.New("PRIVATE_COPY_FAILURE_SENTINEL")
}

func TestPublishMissingCleansInterruptedAndShortCopiesWithoutPublishing(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		input func() io.Reader
	}{
		{"interrupted", func() io.Reader {
			return io.MultiReader(strings.NewReader("PRIVATE_PARTIAL_BYTES"), interruptedCopyReader{})
		}},
		{"short", func() io.Reader { return strings.NewReader("PRIVATE_PARTIAL_BYTES") }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := ownedTestDirectory(t)
			target := filepath.Join(directory, strings.Repeat("ab", 32)+"-a")
			copied, count, err := publishMissing(fixture.input(), 175, 0600, target)
			if !errors.Is(err, errIO) || err.Error() != "cache_io_failed" || copied || count != 0 {
				t.Fatal("copy failure was published or exposed an unbounded/private error")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed copy exposed a cache filename")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed copy retained its private staging file")
			}
		})
	}
}

func TestPublishMissingDoesNotExposePartialActionMetadata(t *testing.T) {
	directory := ownedTestDirectory(t)
	name, _, payload, _ := goldenAction()
	target := filepath.Join(directory, name)
	ready, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	t.Cleanup(release)
	finished := make(chan publicationResult, 1)
	go func() {
		copied, count, err := publishMissing(&gatedCopyReader{payload: payload, ready: ready, resume: resume}, int64(len(payload)), 0400, target)
		finished <- publicationResult{copied, count, err}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("action copy did not reach its controlled partial boundary")
	}
	if _, err := os.ReadFile(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an action filename was published before its complete 175 bytes")
	}
	release()
	select {
	case result := <-finished:
		if result.err != nil || !result.copied || result.bytes != 175 {
			t.Fatal("complete action metadata was not published")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("action copy did not finish after release")
	}
	actual, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("published action metadata differs from the complete source record")
	}
}

type syntheticModeInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (info syntheticModeInfo) Mode() os.FileMode { return info.mode }

func TestCacheDirectoryRejectsGroupAndWorldWritePermissions(t *testing.T) {
	info, err := os.Stat(ownedTestDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range []os.FileMode{0700, 0750, 0755} {
		if err := ownedDirectoryInfo(syntheticModeInfo{FileInfo: info, mode: os.ModeDir | permission}); err != nil {
			t.Fatal("owned nonshared-write directory was rejected")
		}
	}
	for _, permission := range []os.FileMode{0770, 0702, 0777} {
		if err := ownedDirectoryInfo(syntheticModeInfo{FileInfo: info, mode: os.ModeDir | permission}); !errors.Is(err, errMode) {
			t.Fatal("group/world-writable cache directory was accepted")
		}
	}
}
