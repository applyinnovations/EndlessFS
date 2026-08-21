package drive

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/provider"
)

type boundedStorageMapProvider struct {
	provider.Storage
	calls     []domain.ListRequest
	rootCount int
}

type storageMapListProvider struct {
	provider.Storage
	list func(domain.ListRequest) (domain.ListPage, error)
}

func (p storageMapListProvider) List(_ context.Context, _ domain.Scope, request domain.ListRequest) (domain.ListPage, error) {
	return p.list(request)
}

func storageMapDirectory(path string, size, fileCount int64, version string) domain.Entry {
	parsed := domain.MustParseUserPath(path)
	return domain.Entry{
		Path: parsed, Name: parsed.Name(), Kind: domain.EntryDirectory, Size: size, FileCount: fileCount,
		Version: domain.Version(version), ModifiedAt: time.Unix(1, 0).UTC(),
	}
}

func storageMapFile(path string, size int64, version string) domain.Entry {
	parsed := domain.MustParseUserPath(path)
	return domain.Entry{
		Path: parsed, Name: parsed.Name(), Kind: domain.EntryFile, Size: size, FileCount: 1,
		MediaType: "application/octet-stream", Version: domain.Version(version), ModifiedAt: time.Unix(1, 0).UTC(),
	}
}

func (p *boundedStorageMapProvider) List(_ context.Context, _ domain.Scope, request domain.ListRequest) (domain.ListPage, error) {
	p.calls = append(p.calls, request)
	if request.Directory.IsRoot() {
		rootCount := p.rootCount
		if rootCount == 0 {
			rootCount = 12
		}
		var size int64
		for index := 0; index < rootCount; index++ {
			size += int64(200_000 - index*500)
		}
		returnedCount := min(rootCount, request.PageSize)
		entries := make([]domain.Entry, 0, returnedCount)
		for index := 0; index < returnedCount; index++ {
			entrySize := int64(200_000 - index*500)
			name := fmt.Sprintf("directory-%02d", index)
			entries = append(entries, domain.Entry{
				Path: domain.MustParseUserPath("/" + name), Name: name, Kind: domain.EntryDirectory,
				Size: entrySize, FileCount: 100, Version: domain.Version("version-" + name), ModifiedAt: time.Unix(1, 0).UTC(),
			})
		}
		return domain.ListPage{
			Current: domain.Entry{Path: domain.MustParseUserPath("/"), Name: "Files", Kind: domain.EntryDirectory, Size: size, FileCount: int64(rootCount * 100), Version: "root-version", ModifiedAt: time.Unix(1, 0).UTC()},
			Entries: entries,
		}, nil
	}

	name := request.Directory.Name()
	var directoryIndex int
	if _, err := fmt.Sscanf(name, "directory-%02d", &directoryIndex); err != nil {
		return domain.ListPage{}, err
	}
	directorySize := int64(200_000 - directoryIndex*500)
	version := domain.Version("version-" + name)
	if name == "directory-00" {
		version = "newer-version"
	}
	entryLimit := min(request.PageSize, 70)
	entries := make([]domain.Entry, 0, entryLimit)
	for index := 0; index < entryLimit; index++ {
		childName := fmt.Sprintf("child-%02d.bin", index)
		entries = append(entries, domain.Entry{
			Path: domain.MustParseUserPath(request.Directory.String() + "/" + childName), Name: childName, Kind: domain.EntryFile,
			Size: 100, FileCount: 1, MediaType: "application/octet-stream", Version: domain.Version(fmt.Sprintf("child-version-%d", index)), ModifiedAt: time.Unix(1, 0).UTC(),
		})
	}
	return domain.ListPage{
		Current: domain.Entry{Path: request.Directory, Name: name, Kind: domain.EntryDirectory, Size: directorySize, FileCount: 100, Version: version, ModifiedAt: time.Unix(1, 0).UTC()},
		Entries: entries,
	}, nil
}

func TestStorageMapReportsTheLargestEntryOmittedByItsResponseBound(t *testing.T) {
	provider := &boundedStorageMapProvider{rootCount: storageMapRootEntries + 10}
	service := &Service{storage: provider}
	userID, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x24}, 16)))
	if err != nil {
		t.Fatal(err)
	}

	page, err := service.StorageMap(context.Background(), userID, domain.MustParseUserPath("/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != storageMapRootEntries {
		t.Fatalf("storage map root entries = %d, want %d", len(page.Entries), storageMapRootEntries)
	}
	want := int64(200_000 - storageMapRootEntries*500)
	if page.RemainingMaximumSize == nil || *page.RemainingMaximumSize != want {
		t.Fatalf("remaining maximum size = %v, want %d", page.RemainingMaximumSize, want)
	}
}

func TestStorageMapBuildsOneBoundedSnapshotCheckedHierarchy(t *testing.T) {
	provider := &boundedStorageMapProvider{}
	service := &Service{storage: provider}
	userID, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 16)))
	if err != nil {
		t.Fatal(err)
	}

	page, err := service.StorageMap(context.Background(), userID, domain.MustParseUserPath("/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 12 || page.Current.Path != domain.MustParseUserPath("/") {
		t.Fatalf("storage map root = %+v", page)
	}
	if len(page.Entries[0].Children) != 0 {
		t.Fatalf("changed directory snapshot was expanded: %+v", page.Entries[0])
	}
	if len(page.Entries[1].Children) != storageMapChildrenPerDirectory {
		t.Fatalf("first stable directory children = %d, want %d", len(page.Entries[1].Children), storageMapChildrenPerDirectory)
	}
	if page.Entries[1].RemainingMaximumSize == nil || *page.Entries[1].RemainingMaximumSize != 100 {
		t.Fatalf("child remaining maximum size = %v, want 100", page.Entries[1].RemainingMaximumSize)
	}
	nodes := len(page.Entries)
	for _, entry := range page.Entries {
		nodes += len(entry.Children)
	}
	if nodes > storageMapNodeLimit {
		t.Fatalf("storage map returned %d nodes, limit %d", nodes, storageMapNodeLimit)
	}
	if len(provider.calls) != 1+storageMapExpandedDirectoryLimit {
		t.Fatalf("storage map list calls = %d, want %d", len(provider.calls), 1+storageMapExpandedDirectoryLimit)
	}
	if provider.calls[0].PageSize != storageMapRootEntries+1 || provider.calls[0].Sort != domain.SortSize || !provider.calls[0].Descending {
		t.Fatalf("root request = %+v", provider.calls[0])
	}
	for _, request := range provider.calls[1:] {
		if request.PageSize > storageMapChildrenPerDirectory+1 || request.Sort != domain.SortSize || !request.Descending {
			t.Fatalf("unbounded child request = %+v", request)
		}
	}
}

func TestStorageMapFailsClosedForInvalidScopesAndProviderPages(t *testing.T) {
	root := storageMapDirectory("/", 100, 1, "root-version")
	directory := storageMapDirectory("/directory", 100, 1, "directory-version")
	validUserID, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 16)))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := (&Service{}).StorageMap(context.Background(), domain.UserID{}, domain.MustParseUserPath("/")); err == nil {
		t.Fatal("invalid owner scope was accepted")
	}

	tests := []struct {
		name    string
		list    func(domain.ListRequest) (domain.ListPage, error)
		wantErr bool
	}{
		{
			name: "root provider failure",
			list: func(domain.ListRequest) (domain.ListPage, error) {
				return domain.ListPage{}, domain.NewError(domain.ErrorUnavailable, "list unavailable")
			},
			wantErr: true,
		},
		{
			name: "invalid root",
			list: func(domain.ListRequest) (domain.ListPage, error) {
				return domain.ListPage{}, nil
			},
			wantErr: true,
		},
		{
			name: "invalid root child",
			list: func(domain.ListRequest) (domain.ListPage, error) {
				invalid := storageMapFile("/file.bin", 100, "file-version")
				invalid.Name = "different.bin"
				return domain.ListPage{Current: root, Entries: []domain.Entry{invalid}}, nil
			},
			wantErr: true,
		},
		{
			name: "removed child directory",
			list: func(request domain.ListRequest) (domain.ListPage, error) {
				if request.Directory.IsRoot() {
					return domain.ListPage{Current: root, Entries: []domain.Entry{directory}}, nil
				}
				return domain.ListPage{}, domain.NewError(domain.ErrorNotFound, "directory removed")
			},
		},
		{
			name: "child provider failure",
			list: func(request domain.ListRequest) (domain.ListPage, error) {
				if request.Directory.IsRoot() {
					return domain.ListPage{Current: root, Entries: []domain.Entry{directory}}, nil
				}
				return domain.ListPage{}, domain.NewError(domain.ErrorUnavailable, "list unavailable")
			},
			wantErr: true,
		},
		{
			name: "unbounded child page",
			list: func(request domain.ListRequest) (domain.ListPage, error) {
				if request.Directory.IsRoot() {
					return domain.ListPage{Current: root, Entries: []domain.Entry{directory}}, nil
				}
				entries := make([]domain.Entry, storageMapChildrenPerDirectory+2)
				for index := range entries {
					entries[index] = storageMapFile(fmt.Sprintf("/directory/file-%02d.bin", index), 1, fmt.Sprintf("file-version-%02d", index))
				}
				return domain.ListPage{Current: directory, Entries: entries}, nil
			},
			wantErr: true,
		},
		{
			name: "invalid nested child",
			list: func(request domain.ListRequest) (domain.ListPage, error) {
				if request.Directory.IsRoot() {
					return domain.ListPage{Current: root, Entries: []domain.Entry{directory}}, nil
				}
				invalid := storageMapFile("/outside.bin", 1, "outside-version")
				return domain.ListPage{Current: directory, Entries: []domain.Entry{invalid}}, nil
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{storage: storageMapListProvider{list: test.list}}
			page, err := service.StorageMap(context.Background(), validUserID, domain.MustParseUserPath("/"))
			if test.wantErr && err == nil {
				t.Fatalf("StorageMap() = %+v, want error", page)
			}
			if !test.wantErr && (err != nil || len(page.Entries) != 1 || len(page.Entries[0].Children) != 0) {
				t.Fatalf("StorageMap() = %+v, %v; want unexpanded removed directory", page, err)
			}
		})
	}
}
