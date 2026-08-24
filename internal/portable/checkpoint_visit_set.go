package portable

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
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
	root      *os.Root
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
	root, err := os.OpenRoot(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, domain.WrapError(domain.ErrorUnavailable, "open checkpoint visited workspace", err)
	}
	return &checkpointVisitSet{directory: directory, root: root, buffer: make(map[[sha256.Size]byte]struct{}, checkpointVisitBufferEntries), limit: checkpointVisitBufferEntries}, nil
}

func (set *checkpointVisitSet) Close() error {
	if set == nil || set.directory == "" {
		return nil
	}
	rootErr := set.root.Close()
	removeErr := os.RemoveAll(set.directory)
	set.root = nil
	set.directory = ""
	return errors.Join(rootErr, removeErr)
}

func (set *checkpointVisitSet) Seen(value string) (bool, error) {
	if set == nil || set.root == nil || set.directory == "" || value == "" {
		return false, domain.NewError(domain.ErrorInvalid, "invalid checkpoint visited identity")
	}
	digest := sha256.Sum256([]byte(value))
	if _, found := set.buffer[digest]; found {
		return true, nil
	}
	for _, name := range set.levels {
		if name == "" {
			continue
		}
		found, err := checkpointVisitRunContains(set.root, name, digest)
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
	name := checkpointReachabilityChunkName(set.sequence) + ".visited"
	set.sequence++
	file, err := set.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
	return set.insertRun(0, name)
}

func (set *checkpointVisitSet) insertRun(level int, name string) error {
	for len(set.levels) <= level {
		set.levels = append(set.levels, "")
	}
	if set.levels[level] == "" {
		set.levels[level] = name
		return nil
	}
	merged := "merged-" + checkpointReachabilityChunkName(set.sequence) + ".visited"
	set.sequence++
	if err := mergeCheckpointVisitRuns(set.root, set.levels[level], name, merged); err != nil {
		return err
	}
	if err := set.root.Remove(set.levels[level]); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "remove checkpoint visited run", err)
	}
	if err := set.root.Remove(name); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "remove checkpoint visited run", err)
	}
	set.levels[level] = ""
	return set.insertRun(level+1, merged)
}

func checkpointVisitRunContains(root *os.Root, name string, value [sha256.Size]byte) (bool, error) {
	if root == nil {
		return false, domain.NewError(domain.ErrorInvalid, "checkpoint visited workspace is closed")
	}
	file, err := root.Open(name)
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

func openCheckpointVisitRun(root *os.Root, name string) (*checkpointVisitRunReader, error) {
	if root == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "checkpoint visited workspace is closed")
	}
	file, err := root.Open(name)
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

func mergeCheckpointVisitRuns(root *os.Root, leftName, rightName, outputName string) error {
	left, err := openCheckpointVisitRun(root, leftName)
	if err != nil {
		return err
	}
	defer left.file.Close()
	right, err := openCheckpointVisitRun(root, rightName)
	if err != nil {
		return err
	}
	defer right.file.Close()
	output, err := root.OpenFile(outputName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
