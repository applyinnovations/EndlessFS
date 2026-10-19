package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The initial absolute selection is authorized by the immutable Nix config or
// the caller's validated local/scratch path. Subsequent file I/O is Root-relative.
// This is not a defense against malicious same-UID replacement during selection.
func evidenceLocation(path string) (string, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 || path == "/" {
		return "", "", errContract
	}
	if storePath(path) {
		parts := strings.Split(strings.TrimPrefix(path, "/nix/store/"), "/")
		if len(parts) == 1 {
			return "/nix/store", parts[0], nil
		}
		return filepath.Join("/nix/store", parts[0]), strings.Join(parts[1:], "/"), nil
	}
	if strings.HasPrefix(path, "/proc/") {
		parts := strings.Split(strings.TrimPrefix(path, "/proc/"), "/")
		if len(parts) != 2 || (parts[1] != "stat" && parts[1] != "environ") {
			return "", "", errContract
		}
		pid, e := strconv.Atoi(parts[0])
		if e != nil || pid <= 0 || strconv.Itoa(pid) != parts[0] {
			return "", "", errContract
		}
		return "/proc", strings.Join(parts, "/"), nil
	}
	if !safeParents(path, false) {
		return "", "", errContract
	}
	return filepath.Dir(path), filepath.Base(path), nil
}
func openEvidenceRoot(path string) (*os.Root, string, error) {
	anchor, name, e := evidenceLocation(path)
	if e != nil {
		return nil, "", e
	}
	// #nosec G703 -- Initial canonical absolute root metadata only; Nix roots are immutable trusted inputs, local ancestors were checked by safeParents, and proc is a fixed kernel root. Child I/O uses the opened Root below. Same-UID parent replacement during selection is outside this local trust contract.
	info, e := os.Lstat(anchor)
	if e != nil {
		return nil, "", e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", errContract
	}
	root, e := os.OpenRoot(anchor)
	if e != nil {
		return nil, "", e
	}
	return root, name, nil
}
func openRootedEvidence(root *os.Root, name string, flag int, mode fs.FileMode, allowToolLinks bool) (*os.File, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, errContract
	}
	if flag != os.O_RDONLY && flag != os.O_CREATE|os.O_EXCL|os.O_WRONLY {
		return nil, errContract
	}
	if mode&^fs.FileMode(0777) != 0 || mode.Perm()&0022 != 0 {
		return nil, errContract
	}
	var e error
	if flag == os.O_RDONLY {
		var info fs.FileInfo
		// Internal relative tool links stay inside their immutable store ROOT, not
		// merely bin/. Other selected files must themselves be nonsymlink regulars.
		if allowToolLinks {
			info, e = root.Stat(name)
		} else {
			info, e = root.Lstat(name)
		}
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, errContract
		}
	} else {
		if !allowToolLinks {
			info, err := root.Stat(".")
			if err != nil || !owned(info) {
				return nil, errContract
			}
		}
	}
	return root.OpenFile(name, flag, mode)
}

// Nix tool outputs may be absolute symlinks to another immutable store object.
// Resolve that trusted initial READ selection, require the target to stay in the
// store, and then confine all subsequent I/O to the resolved object's Root.
// Local evidence and every write retain the original nonsymlink selection.
func resolveImmutableRead(path string, resolve func(string) (string, error)) (string, error) {
	if !storePath(path) || strings.IndexByte(path, 0) >= 0 {
		return "", errContract
	}
	target, e := resolve(path)
	if e != nil || !storePath(target) || strings.IndexByte(target, 0) >= 0 {
		return "", errContract
	}
	return target, nil
}
func openEvidenceFile(path string, flag int, mode fs.FileMode) (*os.File, error) {
	if flag == os.O_RDONLY && storePath(path) {
		var e error
		path, e = resolveImmutableRead(path, filepath.EvalSymlinks)
		if e != nil {
			return nil, e
		}
	}
	root, name, e := openEvidenceRoot(path)
	if e != nil {
		return nil, e
	}
	if strings.HasPrefix(path, "/proc/") && flag != os.O_RDONLY {
		_ = root.Close()
		return nil, errContract
	}
	file, e := openRootedEvidence(root, name, flag, mode, storePath(path))
	closeErr := root.Close()
	if e != nil {
		return nil, e
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, closeErr
	}
	return file, nil
}
func makeEvidenceDirectory(path string) error {
	root, name, e := openEvidenceRoot(path)
	if e != nil {
		return e
	}
	if !storePath(path) {
		info, e := root.Stat(".")
		if e != nil || !owned(info) {
			_ = root.Close()
			return errContract
		}
	}
	e = root.Mkdir(name, 0700)
	closeErr := root.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func checkedWriteCount(n, expected int) uint64 {
	if n < 0 || n != expected {
		panic(errContract)
	}
	return uint64(n)
}
func secureScratchDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errContract
	}
	info, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned(info) {
		return errContract
	}
	// #nosec G302 -- This is a verified freshly owned DIRECTORY from MkdirTemp, never a data file; owner-only 0700 traversal is required. Keep the pathname chmod so even an inherited umask producing 0000 is repaired before Root/FD opening; no group/other permission is granted.
	return os.Chmod(path, 0700)
}
