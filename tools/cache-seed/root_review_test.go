package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Every source/target below is synthetic test-owned scratch. Existing golden,
// concurrent publication, mode and interrupted-copy controls remain unchanged.
func TestCacheRootRejectsInsideLinksAndSpecialSource(t *testing.T) {
	for _, fixture := range []string{"source_shard", "source_file", "source_data_child", "destination_shard", "destination_file", "fifo"} {
		t.Run(fixture, func(t *testing.T) {
			source, actionPath, dataPath := seedFixture(t)
			if _, err := normalize(source); err != nil {
				t.Fatal(err)
			}
			destination := ownedTestDirectory(t)
			var link, target string
			switch fixture {
			case "source_shard":
				shard := filepath.Dir(actionPath)
				moved := filepath.Join(source, "cd")
				if err := os.Rename(shard, moved); err != nil {
					t.Fatal(err)
				}
				link, target = shard, "cd"
			case "source_file":
				if err := os.Remove(dataPath); err != nil {
					t.Fatal(err)
				}
				link, target = dataPath, "../"+filepath.Base(filepath.Dir(actionPath))+"/"+filepath.Base(actionPath)
			case "source_data_child":
				if err := os.Remove(dataPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dataPath, 0700); err != nil {
					t.Fatal(err)
				}
				link, target = filepath.Join(dataPath, "executable"), "../../"+filepath.Base(filepath.Dir(actionPath))+"/"+filepath.Base(actionPath)
			case "destination_shard":
				if err := os.Mkdir(filepath.Join(destination, "cd"), 0700); err != nil {
					t.Fatal(err)
				}
				link, target = filepath.Join(destination, filepath.Base(filepath.Dir(actionPath))), "cd"
			case "destination_file":
				shard := filepath.Join(destination, filepath.Base(filepath.Dir(actionPath)))
				if err := os.Mkdir(shard, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(destination, "README"), []byte("UNCHANGED"), 0600); err != nil {
					t.Fatal(err)
				}
				link, target = filepath.Join(shard, filepath.Base(actionPath)), "../README"
			case "fifo":
				if err := os.Remove(dataPath); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(dataPath, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if link != "" {
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := install(source, destination); !errors.Is(err, errTree) {
				t.Fatal("link or special source accepted")
			}
		})
	}
}

func TestOpenedRootRejectsEscapesAndKeepsOriginalDirectoryAfterRename(t *testing.T) {
	parent := ownedTestDirectory(t)
	name := filepath.Join(parent, "cache")
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := openCacheRoot(name, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, target := range []string{"", ".", "../outside", "aa/../../outside", filepath.Join(parent, "outside")} {
		copied, count, err := publishMissingRoot(root, bytes.NewReader([]byte("DATA")), 4, 0400, target)
		if !errors.Is(err, errTree) || copied || count != 0 {
			t.Fatal("nonlocal relative target accepted")
		}
	}
	if err := root.Symlink("../outside", "linked"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rootTemporary(root, "linked", ".owned-"); !errors.Is(err, errTree) {
		t.Fatal("rooted staging followed escape")
	}
	if err := root.Remove("linked"); err != nil {
		t.Fatal(err)
	}
	// A pathname implementation would now write into the replacement. Root
	// methods must retain the ORIGINAL opened directory, including cleanup.
	moved := filepath.Join(parent, "original-open-root")
	if err := os.Rename(name, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(name, "sentinel"), []byte("UNCHANGED"), 0400); err != nil {
		t.Fatal(err)
	}
	copied, count, err := publishMissingRoot(root, bytes.NewReader([]byte("DATA")), 4, 0400, "entry")
	if err != nil || !copied || count != 4 {
		t.Fatal("opened root lost its directory identity")
	}
	actual, err := os.ReadFile(filepath.Join(moved, "entry"))
	entries, readErr := os.ReadDir(name)
	movedEntries, movedErr := os.ReadDir(moved)
	if err != nil || string(actual) != "DATA" || readErr != nil || len(entries) != 1 || entries[0].Name() != "sentinel" ||
		movedErr != nil || len(movedEntries) != 1 || movedEntries[0].Name() != "entry" {
		t.Fatal("child I/O used a replaced root pathname or retained staging")
	}
}

func TestRootPublicationPreservesExistingWinnerAndDeniesLinksOrDirectory(t *testing.T) {
	for _, fixture := range []string{"regular", "link", "directory", "ancestor_link"} {
		t.Run(fixture, func(t *testing.T) {
			name := ownedTestDirectory(t)
			root, err := openCacheRoot(name, false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.WriteFile("winner", []byte("WINNER"), 0400); err != nil {
				t.Fatal(err)
			}
			target := "winner"
			switch fixture {
			case "link":
				if err := root.Symlink("winner", "target"); err != nil {
					t.Fatal(err)
				}
				target = "target"
			case "directory":
				if err := root.Mkdir("target", 0700); err != nil {
					t.Fatal(err)
				}
				target = "target"
			case "ancestor_link":
				if err := root.MkdirAll("real/child", 0700); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("real", "linked"); err != nil {
					t.Fatal(err)
				}
				target = "linked/child/target"
			}
			copied, count, err := publishMissingRoot(root, strings.NewReader("LOSER"), 5, 0600, target)
			if fixture == "regular" {
				if err != nil || copied || count != 0 {
					t.Fatal("regular atomic winner was not preserved")
				}
			} else if !errors.Is(err, errTree) || copied || count != 0 {
				t.Fatal("link/directory target accepted")
			}
			actual, err := root.ReadFile("winner")
			info, statErr := root.Lstat("winner")
			if err != nil || string(actual) != "WINNER" || statErr != nil || info.Mode().Perm() != 0400 {
				t.Fatal("existing winner changed")
			}
			if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if strings.HasPrefix(entry.Name(), ".endlessfs-seed-") {
					t.Fatal("publication left a staging file")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type rootEntropyReader func([]byte) (int, error)

func (reader rootEntropyReader) Read(body []byte) (int, error) { return reader(body) }

func TestRootTemporaryEntropyAndCollisionFailuresAreBoundedAndPrivate(t *testing.T) {
	for _, fixture := range []string{"error", "zero_progress", "short", "collision"} {
		t.Run(fixture, func(t *testing.T) {
			root, err := openCacheRoot(ownedTestDirectory(t), false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			calls := 0
			entropy := rootEntropyReader(func(body []byte) (int, error) {
				calls++
				switch fixture {
				case "error":
					return 0, errors.New("PRIVATE_ENTROPY_ERROR")
				case "zero_progress":
					return 0, nil
				case "short":
					return copy(body, []byte{1}), nil
				default:
					clear(body)
					return len(body), nil
				}
			})
			collision := ".owned-" + strings.Repeat("0", 32)
			if fixture == "collision" {
				if err := root.WriteFile(collision, []byte("ORIGINAL"), 0400); err != nil {
					t.Fatal(err)
				}
			}
			file, relative, err := rootTemporaryWithEntropy(root, ".", ".owned-", entropy)
			if !errors.Is(err, errIO) || err.Error() != "cache_io_failed" || file != nil || relative != "" {
				t.Fatal("entropy failure leaked or issued a staging file")
			}
			wantCalls, wantEntries := 1, 0
			if fixture == "collision" {
				wantCalls, wantEntries = 100, 1
			}
			entries, readErr := fs.ReadDir(root.FS(), ".")
			if calls != wantCalls || readErr != nil || len(entries) != wantEntries {
				t.Fatal("entropy retry or cleanup was unbounded")
			}
			if fixture == "collision" {
				body, err := root.ReadFile(collision)
				if err != nil || string(body) != "ORIGINAL" {
					t.Fatal("O_EXCL collision modified existing data")
				}
			}
		})
	}
}

func TestNormalizerRejectsInsideLinkAndUnownedModeDirectory(t *testing.T) {
	for _, fixture := range []string{"inside_link", "group_write"} {
		t.Run(fixture, func(t *testing.T) {
			root, actionPath, dataPath := seedFixture(t)
			before, err := os.ReadFile(dataPath)
			if err != nil {
				t.Fatal(err)
			}
			shard := filepath.Dir(actionPath)
			if fixture == "inside_link" {
				moved := filepath.Join(root, "cd")
				if err := os.Rename(shard, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("cd", shard); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(shard, 0770); err != nil {
				t.Fatal(err)
			}
			_, err = normalize(root)
			if fixture == "inside_link" && !errors.Is(err, errTree) || fixture == "group_write" && !errors.Is(err, errMode) {
				t.Fatal("unsafe normalization directory accepted")
			}
			after, err := os.ReadFile(dataPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("normalization denial modified archive data")
			}
		})
	}
}

func TestRootPublicationCancellationCleansStagedPrefix(t *testing.T) {
	root, err := openCacheRoot(ownedTestDirectory(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	input := io.MultiReader(strings.NewReader("PRIVATE_PARTIAL"), rootEntropyReader(func([]byte) (int, error) {
		return 0, context.Canceled
	}))
	copied, count, err := publishMissingRoot(root, input, 175, 0400, "entry")
	entries, readErr := fs.ReadDir(root.FS(), ".")
	if !errors.Is(err, errIO) || err.Error() != "cache_io_failed" || copied || count != 0 || readErr != nil || len(entries) != 0 {
		t.Fatal("reader cancellation retained or published partial bytes")
	}
}

// Compile-time interface assertion: failure seams remain ordinary Readers;
// production uses crypto/rand.Reader directly, with no mutable global override.
var _ io.Reader = rootEntropyReader(nil)
