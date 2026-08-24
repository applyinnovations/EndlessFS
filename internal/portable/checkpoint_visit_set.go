package portable

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const checkpointVisitBufferEntries = 16 * 1024

// checkpointVisitSet is a bounded-memory exact set over SHA-256 identities.
// Its immutable sorted runs form a binary LSM: there is at most one run per
// level, so lookup opens O(log n) files and insertion periodically performs a
// streaming two-way merge. SHA-256 is already the canonical identity primitive
// for every page being visited; using the same identity here does not add a
// weaker collision boundary.
type checkpointVisitSet struct {
	directory string
	buffer    map[[sha256.Size]byte]struct{}
	levels    []string
	sequence  uint64
	limit     int
}

func newCheckpointVisitSet() (*checkpointVisitSet, error) {
	directory, err := os.MkdirTemp("", "endlessfs-checkpoint-visited-")
	if err != nil {
		return nil, domain.WrapError(domain.ErrorUnavailable, "create checkpoint visited workspace", err)
	}
	return &checkpointVisitSet{directory: directory, buffer: make(map[[sha256.Size]byte]struct{}, checkpointVisitBufferEntries), limit: checkpointVisitBufferEntries}, nil
}

func (set *checkpointVisitSet) Close() error {
	if set == nil || set.directory == "" {
		return nil
	}
	err := os.RemoveAll(set.directory)
	set.directory = ""
	return err
}

func (set *checkpointVisitSet) Seen(value string) (bool, error) {
	if set == nil || set.directory == "" || value == "" {
		return false, domain.NewError(domain.ErrorInvalid, "invalid checkpoint visited identity")
	}
	digest := sha256.Sum256([]byte(value))
	if _, found := set.buffer[digest]; found {
		return true, nil
	}
	for _, path := range set.levels {
		if path == "" {
			continue
		}
		found, err := checkpointVisitRunContains(path, digest)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	set.buffer[digest] = struct{}{}
	if len(set.buffer) >= set.limit {
		if err := set.flush(); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (set *checkpointVisitSet) flush() error {
	if len(set.buffer) == 0 {
		return nil
	}
	values := make([][sha256.Size]byte, 0, len(set.buffer))
	for value := range set.buffer {
		values = append(values, value)
	}
	sort.Slice(values, func(left, right int) bool { return bytes.Compare(values[left][:], values[right][:]) < 0 })
	path := filepath.Join(set.directory, checkpointReachabilityChunkName(set.sequence)+".visited")
	set.sequence++
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "create checkpoint visited run", err)
	}
	for _, value := range values {
		if _, err := file.Write(value[:]); err != nil {
			_ = file.Close()
			return domain.WrapError(domain.ErrorUnavailable, "write checkpoint visited run", err)
		}
	}
	if err := file.Close(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "close checkpoint visited run", err)
	}
	clear(set.buffer)
	return set.insertRun(0, path)
}

func (set *checkpointVisitSet) insertRun(level int, path string) error {
	for len(set.levels) <= level {
		set.levels = append(set.levels, "")
	}
	if set.levels[level] == "" {
		set.levels[level] = path
		return nil
	}
	merged := filepath.Join(set.directory, "merged-"+checkpointReachabilityChunkName(set.sequence)+".visited")
	set.sequence++
	if err := mergeCheckpointVisitRuns(set.levels[level], path, merged); err != nil {
		return err
	}
	if err := os.Remove(set.levels[level]); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "remove checkpoint visited run", err)
	}
	if err := os.Remove(path); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "remove checkpoint visited run", err)
	}
	set.levels[level] = ""
	return set.insertRun(level+1, merged)
}

func checkpointVisitRunContains(path string, value [sha256.Size]byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, domain.WrapError(domain.ErrorUnavailable, "open checkpoint visited run", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size()%sha256.Size != 0 {
		return false, domain.NewError(domain.ErrorInvalid, "invalid checkpoint visited run")
	}
	left, right := int64(0), info.Size()/sha256.Size
	var candidate [sha256.Size]byte
	for left < right {
		middle := left + (right-left)/2
		if _, err := file.ReadAt(candidate[:], middle*sha256.Size); err != nil {
			return false, domain.WrapError(domain.ErrorUnavailable, "read checkpoint visited run", err)
		}
		if bytes.Compare(candidate[:], value[:]) < 0 {
			left = middle + 1
		} else {
			right = middle
		}
	}
	if left == info.Size()/sha256.Size {
		return false, nil
	}
	if _, err := file.ReadAt(candidate[:], left*sha256.Size); err != nil {
		return false, domain.WrapError(domain.ErrorUnavailable, "read checkpoint visited run", err)
	}
	return candidate == value, nil
}

type checkpointVisitRunReader struct {
	file    *os.File
	current [sha256.Size]byte
	found   bool
}

func openCheckpointVisitRun(path string) (*checkpointVisitRunReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, domain.WrapError(domain.ErrorUnavailable, "open checkpoint visited merge input", err)
	}
	reader := &checkpointVisitRunReader{file: file}
	if err := reader.advance(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return reader, nil
}

func (reader *checkpointVisitRunReader) advance() error {
	_, err := io.ReadFull(reader.file, reader.current[:])
	if errors.Is(err, io.EOF) {
		reader.found = false
		return nil
	}
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "read checkpoint visited merge input", err)
	}
	reader.found = true
	return nil
}

func mergeCheckpointVisitRuns(leftPath, rightPath, outputPath string) error {
	left, err := openCheckpointVisitRun(leftPath)
	if err != nil {
		return err
	}
	defer left.file.Close()
	right, err := openCheckpointVisitRun(rightPath)
	if err != nil {
		return err
	}
	defer right.file.Close()
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "create checkpoint visited merged run", err)
	}
	defer output.Close()
	var previous [sha256.Size]byte
	havePrevious := false
	for left.found || right.found {
		var value [sha256.Size]byte
		advanceLeft, advanceRight := false, false
		switch {
		case !right.found || left.found && bytes.Compare(left.current[:], right.current[:]) < 0:
			value, advanceLeft = left.current, true
		case !left.found || bytes.Compare(right.current[:], left.current[:]) < 0:
			value, advanceRight = right.current, true
		default:
			value, advanceLeft, advanceRight = left.current, true, true
		}
		if !havePrevious || value != previous {
			if _, err := output.Write(value[:]); err != nil {
				return domain.WrapError(domain.ErrorUnavailable, "write checkpoint visited merged run", err)
			}
			previous, havePrevious = value, true
		}
		if advanceLeft {
			if err := left.advance(); err != nil {
				return err
			}
		}
		if advanceRight {
			if err := right.advance(); err != nil {
				return err
			}
		}
	}
	return output.Sync()
}
