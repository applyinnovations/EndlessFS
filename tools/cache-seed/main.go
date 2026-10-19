// Command cache-seed handles disposable pinned-Go compiler-cache metadata.
// Normalize changes only action timestamps in an idle build cache. Install
// never edits source, compiled artifact contents, or an existing cache file.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

var (
	errArguments = errors.New("invalid_arguments")
	errTree      = errors.New("invalid_cache_tree")
	errAction    = errors.New("invalid_action_metadata")
	errOwner     = errors.New("unowned_cache_destination")
	errMode      = errors.New("unsafe_cache_permissions")
	errIO        = errors.New("cache_io_failed")
)

type summary struct {
	Schema      int    `json:"schema"`
	Actions     uint64 `json:"action_entries"`
	Normalized  uint64 `json:"normalized_entries"`
	Copied      uint64 `json:"copied_files"`
	Existing    uint64 `json:"preserved_files"`
	Bytes       uint64 `json:"copied_bytes"`
	TrimRemoved bool   `json:"trim_removed"`
}

// Go 1.26 cache/cache.go:139 defines 175 bytes, including final newline:
// v1 <64hex action> <64hex output> <20byte size> <20byte UnixNano>\n.
// Only the timestamp is changed. No output/archive bytes are interpreted.
func normalizedAction(name string, body []byte) ([]byte, bool, error) {
	if len(name) != 66 || !strings.HasSuffix(name, "-a") || !lowerHex(name[:64], 64) || len(body) != 175 ||
		string(body[:3]) != "v1 " || body[67] != ' ' || body[132] != ' ' || body[153] != ' ' || body[174] != '\n' ||
		string(body[3:67]) != name[:64] || !lowerHex(string(body[68:132]), 64) {
		return nil, false, errAction
	}
	for _, field := range [][]byte{body[133:153], body[154:174]} {
		text := strings.TrimLeft(string(field), " ")
		if text == "" {
			return nil, false, errAction
		}
		for _, digit := range text {
			if digit < '0' || digit > '9' {
				return nil, false, errAction
			}
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil || fmt.Sprintf("%20d", value) != string(field) {
			return nil, false, errAction
		}
	}
	zeroTime := []byte("                   0")
	if bytes.Equal(body[154:174], zeroTime) {
		return append([]byte(nil), body...), false, nil
	}
	result := append([]byte(nil), body...)
	copy(result[154:174], zeroTime)
	return result, true, nil
}

func lowerHex(text string, length int) bool {
	if len(text) != length || strings.ToLower(text) != text {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

func ownedDirectoryInfo(info fs.FileInfo) error {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errTree
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	uid := os.Getuid()
	if !ok || uid < 0 || uint64(stat.Uid) != uint64(uid) {
		return errOwner
	}
	if info.Mode().Perm()&0022 != 0 {
		return errMode
	}
	return nil
}

// Cache layout is pinned too: 256 hex shards, action/data entries, README and
// optional trim.txt. Executable data entries may be directories. Symlinks and
// special files are rejected before copying or rewriting through them.
func walkOpenedCache(root *os.Root, visit func(string, fs.DirEntry) error) error {
	return fs.WalkDir(root.FS(), ".", func(relative string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errIO
		}
		if relative == "." {
			return nil
		}
		if !fs.ValidPath(relative) || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
			return errTree
		}
		parts := strings.Split(relative, "/")
		if len(parts) == 1 && (relative == "README" || relative == "trim.txt") && !entry.IsDir() {
			return visit(relative, entry)
		}
		if !lowerHex(parts[0], 2) || len(parts) == 1 && !entry.IsDir() {
			return errTree
		}
		if len(parts) > 1 {
			name := parts[1]
			if len(name) != 66 || !lowerHex(name[:64], 64) || name[:2] != parts[0] || name[64] != '-' || name[65] != 'a' && name[65] != 'd' ||
				strings.HasSuffix(name, "-a") && (entry.IsDir() || len(parts) != 2) || len(parts) > 3 || len(parts) == 3 && entry.IsDir() {
				return errTree
			}
		}
		return visit(relative, entry)
	})
}

func normalize(root string) (summary, error) {
	result := summary{Schema: 1}
	opened, err := openCacheRoot(root, false, true)
	if err != nil {
		return result, err
	}
	defer opened.Close()
	err = walkOpenedCache(opened, func(relative string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return ownedRootDirectory(opened, relative)
		}
		if strings.Count(relative, "/") != 1 || !strings.HasSuffix(entry.Name(), "-a") {
			return nil
		}
		file, err := opened.Open(relative)
		if err != nil {
			return errIO
		}
		body, readErr := io.ReadAll(io.LimitReader(file, 176))
		info, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || statErr != nil || closeErr != nil {
			return errIO
		}
		if !info.Mode().IsRegular() {
			return errTree
		}
		canonical, changed, err := normalizedAction(entry.Name(), body)
		if err != nil {
			return err
		}
		result.Actions++
		if !changed {
			return nil
		}
		// Atomic replacement permits read-only action files without modifying
		// their mode or any compiled output. The private build cache is idle.
		staged, name, err := rootTemporary(opened, path.Dir(relative), ".cache-seed-action-")
		if err != nil {
			return errIO
		}
		defer opened.Remove(name)
		if count, err := staged.Write(canonical); err != nil || count != len(canonical) {
			_ = staged.Close()
			return errIO
		}
		modeErr, finishErr := staged.Chmod(info.Mode().Perm()), staged.Close()
		if modeErr != nil || finishErr != nil || opened.Rename(name, relative) != nil {
			return errIO
		}
		result.Normalized++
		return nil
	})
	if err != nil {
		return result, err
	}
	if err := opened.Remove("trim.txt"); err == nil {
		result.TrimRemoved = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return result, errIO
	}
	return result, nil
}

// Initial root selection is an explicit CLI boundary, not a child-path API.
// Canonical absolute names and EVERY existing ancestor must be non-symlink
// directories. This does not defend hostile same-UID replacement before root
// opening; task/Nix parents are trusted. Actual child I/O uses the opened Root.
func cacheRootAnchor(destination string) (string, error) {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return "", errTree
	}
	anchor := ""
	for current := destination; ; current = filepath.Dir(current) {
		// #nosec G703 -- Initial CLI-authorized canonical absolute root/ancestor metadata inspection only; no child I/O. Every existing ancestor is checked below, writable terminal ownership/0022 is checked after OpenRoot. Trusted task parents must not be replaced by a hostile same UID during selection.
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", errTree
			}
			if anchor == "" {
				anchor = current
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", errTree
		}
		if current == filepath.Dir(current) {
			if anchor == "" {
				return "", errTree
			}
			return anchor, nil
		}
	}
}

func openCacheRoot(name string, create, owned bool) (*os.Root, error) {
	anchor, err := cacheRootAnchor(name)
	if err != nil {
		return nil, err
	}
	if !create && anchor != name {
		return nil, errTree
	}
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, errTree
	}
	if anchor != name {
		relative, relativeErr := filepath.Rel(anchor, name)
		relative = filepath.ToSlash(relative)
		if relativeErr != nil || !fs.ValidPath(relative) {
			_ = root.Close()
			return nil, errTree
		}
		if err := root.MkdirAll(relative, 0700); err != nil {
			_ = root.Close()
			return nil, errIO
		}
		child, childErr := root.OpenRoot(relative)
		_ = root.Close()
		if childErr != nil {
			return nil, errTree
		}
		root = child
	}
	info, err := root.Lstat(".")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = root.Close()
		return nil, errTree
	}
	if owned {
		if err := ownedDirectoryInfo(info); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return root, nil
}

func ownedRootDirectory(root *os.Root, name string) error {
	if !fs.ValidPath(name) {
		return errTree
	}
	// Root confines paths but follows links INSIDE its tree. Explicitly reject
	// links in every directory component and retain owned/nonshared permissions.
	for current := name; ; current = path.Dir(current) {
		info, err := root.Lstat(current)
		if err != nil {
			return errTree
		}
		if err := ownedDirectoryInfo(info); err != nil {
			return err
		}
		if current == "." {
			return nil
		}
	}
}

// Retain a ROOT-RELATIVE name separately: File.Name is only diagnostic and may
// contain the root's original pathname. O_EXCL never follows a preexisting leaf.
func rootTemporary(root *os.Root, directory, prefix string) (*os.File, string, error) {
	return rootTemporaryWithEntropy(root, directory, prefix, rand.Reader)
}

func rootTemporaryWithEntropy(root *os.Root, directory, prefix string, entropy io.Reader) (*os.File, string, error) {
	if !fs.ValidPath(directory) || strings.ContainsAny(prefix, "/\\") {
		return nil, "", errTree
	}
	if err := ownedRootDirectory(root, directory); err != nil {
		return nil, "", err
	}
	for range 100 {
		var nonce [16]byte
		// crypto/rand.Reader fills its input. The test seam fails closed on short
		// or zero-progress reads instead of adding an unbounded read/retry loop.
		if count, err := entropy.Read(nonce[:]); err != nil || count != len(nonce) {
			return nil, "", errIO
		}
		name := path.Join(directory, prefix+hex.EncodeToString(nonce[:]))
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", errIO
		}
		return file, name, nil
	}
	return nil, "", errIO
}

func install(source, destination string) (summary, error) {
	result := summary{Schema: 1}
	inputRoot, err := openCacheRoot(source, false, false)
	if err != nil {
		return result, err
	}
	defer inputRoot.Close()
	outputRoot, err := openCacheRoot(destination, true, true)
	if err != nil {
		return result, err
	}
	defer outputRoot.Close()
	err = walkOpenedCache(inputRoot, func(relative string, entry fs.DirEntry) error {
		if relative == "trim.txt" {
			return errTree
		}
		if entry.IsDir() {
			if err := outputRoot.Mkdir(relative, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return errIO
			}
			return ownedRootDirectory(outputRoot, relative)
		}
		if existing, err := outputRoot.Lstat(relative); err == nil {
			if !existing.Mode().IsRegular() {
				return errTree
			}
			result.Existing++
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return errIO
		}
		input, err := inputRoot.Open(relative)
		if err != nil {
			return errIO
		}
		info, err := input.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = input.Close()
			if err != nil {
				return errIO
			}
			return errTree
		}
		copied, count, err := publishMissingRoot(outputRoot, input, info.Size(), info.Mode().Perm(), relative)
		closeErr := input.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return errIO
		}
		if copied {
			result.Copied++
			result.Bytes += count
		} else {
			result.Existing++
		}
		return nil
	})
	return result, err
}

// publishMissing never exposes partially copied bytes at a cache filename.
// A private temporary file in the destination directory is completely copied,
// mode-adjusted and closed before Link publishes it atomically without replace.
// The only temporary hardlink is between two names for this newly owned inode;
// the immutable source is read, never linked. Removing the staging name leaves
// one destination link. The caller uses an owned local cache, not hostile peers.
func publishMissing(input io.Reader, expected int64, mode fs.FileMode, target string) (bool, uint64, error) {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return false, 0, errTree
	}
	root, err := openCacheRoot(filepath.Dir(target), false, true)
	if err != nil {
		return false, 0, err
	}
	defer root.Close()
	return publishMissingRoot(root, input, expected, mode, filepath.Base(target))
}

func publishMissingRoot(root *os.Root, input io.Reader, expected int64, mode fs.FileMode, target string) (bool, uint64, error) {
	if expected < 0 {
		return false, 0, errIO
	}
	if !fs.ValidPath(target) || target == "." {
		return false, 0, errTree
	}
	directory := path.Dir(target)
	if err := ownedRootDirectory(root, directory); err != nil {
		return false, 0, err
	}
	staged, name, err := rootTemporary(root, directory, ".endlessfs-seed-")
	if err != nil {
		return false, 0, errIO
	}
	defer root.Remove(name)
	count, copyErr := io.Copy(staged, input)
	if copyErr != nil || count < 0 || count != expected {
		_ = staged.Close()
		return false, 0, errIO
	}
	if err := staged.Chmod(mode.Perm() | 0200); err != nil {
		_ = staged.Close()
		return false, 0, errIO
	}
	if err := staged.Close(); err != nil {
		return false, 0, errIO
	}
	if err := root.Link(name, target); errors.Is(err, fs.ErrExist) {
		if existing, statErr := root.Lstat(target); statErr != nil || !existing.Mode().IsRegular() {
			return false, 0, errTree
		}
		return false, 0, nil
	} else if err != nil {
		return false, 0, errIO
	}
	return true, uint64(count), nil
}

func main() {
	var result summary
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "normalize":
		result, err = normalize(os.Args[2])
	case len(os.Args) == 4 && os.Args[1] == "install":
		result, err = install(os.Args[2], os.Args[3])
	default:
		err = errArguments
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dependency_compile_cache:"+err.Error())
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "dependency_compile_cache:summary_io_failed")
		os.Exit(1)
	}
}
