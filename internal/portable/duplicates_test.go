package portable_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
)

func TestDuplicateSimilarityPostingsDoNotRewriteWhenContentSetIsUnchanged(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2048, 1, 1, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(139, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 140, nil)
	user, _ := domain.ParseUserID("Z2dnaGdnaGdnaGdnaGdnaA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/copies")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/copies/a.bin"), []byte("same bytes"))
	before := duplicateSimilarityObjects(backend.Export())
	if len(before) != 16 {
		t.Fatalf("user-addressable directory similarity posting count = %d; want 16 and no area-root postings", len(before))
	}
	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/copies/b.bin"), []byte("same bytes"))
	after := duplicateSimilarityObjects(backend.Export())
	if len(before) == 0 || len(after) != len(before) {
		t.Fatalf("similarity posting count changed for duplicate multiplicity: %d -> %d", len(before), len(after))
	}
	for key, body := range before {
		if !bytes.Equal(body, after[key]) {
			t.Fatalf("unchanged content set rewrote similarity posting %s", key)
		}
	}
}

func duplicateSimilarityObjects(objects map[string][]byte) map[string][]byte {
	result := make(map[string][]byte)
	for key, body := range objects {
		if strings.Contains(key, "/duplicates/") && strings.Contains(key, "/similarity/") {
			result[key] = body
		}
	}
	return result
}

func TestDuplicateCatalogTracksFileAndExactDirectoryGroupsIncrementally(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2048, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(141, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 142, nil)
	user, _ := domain.ParseUserID("aGhoaGhoaGhoaGhoaGhoaA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/project-a", "/backup/project-a"} {
		parent := domain.MustParseUserPath(path).Parent()
		if !parent.IsRoot() {
			if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: parent}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
		uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath(path+"/same.txt"), []byte("same bytes"))
	}

	page, err := engine.Files().ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	fileGroup := duplicateGroupByKind(t, page.Groups, domain.DuplicateFile)
	if fileGroup.OccurrenceCount != 2 || fileGroup.Size != 10 || fileGroup.ReclaimableBytes != 10 || fileGroup.Ignored {
		t.Fatalf("file duplicate group = %+v", fileGroup)
	}
	directoryGroup := duplicateGroupByKind(t, page.Groups, domain.DuplicateDirectory)
	if directoryGroup.OccurrenceCount != 2 || directoryGroup.FileCount != 1 || directoryGroup.Size != 10 || directoryGroup.ReclaimableBytes != 10 {
		t.Fatalf("directory duplicate group = %+v", directoryGroup)
	}

	occurrences, err := engine.Files().ListDuplicateOccurrences(context.Background(), user, domain.DuplicateOccurrenceRequest{GroupID: directoryGroup.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicateOccurrencePathsEqual(occurrences.Occurrences, "/backup/project-a", "/project-a") {
		t.Fatalf("directory occurrences = %+v", occurrences.Occurrences)
	}
	overlaps, err := engine.Files().ListDuplicateDirectoryOverlaps(context.Background(), user, domain.DuplicateDirectoryOverlapRequest{
		Directory: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")}, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundExactOverlap := false
	for _, candidate := range overlaps.Candidates {
		if candidate.Comparison.Right.Path.String() == "/backup/project-a" {
			foundExactOverlap = candidate.Comparison.Exact && candidate.SharedSketch == candidate.SketchSize && candidate.SketchSize > 0
		}
	}
	if !foundExactOverlap {
		t.Fatalf("exact overlap candidate is missing: %+v", overlaps.Candidates)
	}
	pairPreference, err := engine.Files().SetDuplicateDirectoryIgnored(context.Background(), user, domain.SetDuplicateDirectoryIgnoredRequest{
		Left:  domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")},
		Right: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")}, Ignored: true,
	})
	if err != nil || !pairPreference.Ignored || pairPreference.Revision != 1 {
		t.Fatalf("pair ignore = %+v, %v", pairPreference, err)
	}
	hidden, err := engine.Files().ListDuplicateDirectoryOverlaps(context.Background(), user, domain.DuplicateDirectoryOverlapRequest{
		Directory: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")}, Limit: 20,
	})
	if err != nil || duplicateOverlapContainsPath(hidden.Candidates, "/backup/project-a") {
		t.Fatalf("pair-ignored overlap remained visible: %+v, %v", hidden, err)
	}
	included, err := engine.Files().ListDuplicateDirectoryOverlaps(context.Background(), user, domain.DuplicateDirectoryOverlapRequest{
		Directory: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")}, Limit: 20, IncludeIgnored: true,
	})
	includedPair, found := duplicateOverlapByPath(included.Candidates, "/backup/project-a")
	if err != nil || !found || !includedPair.Ignored || includedPair.IgnoreRevision != pairPreference.Revision {
		t.Fatalf("included pair ignore = %+v, %v", included, err)
	}
	pairPreference, err = engine.Files().SetDuplicateDirectoryIgnored(context.Background(), user, domain.SetDuplicateDirectoryIgnoredRequest{
		Left:    domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")},
		Right:   domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")},
		Ignored: false, ExpectedRevision: pairPreference.Revision,
	})
	if err != nil || pairPreference.Ignored || pairPreference.Revision != 2 {
		t.Fatalf("pair unignore = %+v, %v", pairPreference, err)
	}
	if _, err := engine.Files().SetDuplicateDirectoryIgnored(context.Background(), user, domain.SetDuplicateDirectoryIgnoredRequest{
		Left:    domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")},
		Right:   domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")},
		Ignored: true, ExpectedRevision: 1,
	}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale duplicate directory ignore error = %v; want precondition failed", err)
	}
	exactPreview, err := engine.Files().PreviewDuplicateReconciliation(context.Background(), user, domain.DuplicateReconciliationPreviewRequest{
		Left:       domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")},
		Right:      domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")},
		RemoveFrom: domain.DuplicateSideRight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exactPreview.Comparison.Exact || len(exactPreview.Items) != 1 || exactPreview.Items[0].Remove.Path.String() != "/backup/project-a" || exactPreview.Items[0].Keep.Path.String() != "/project-a" || exactPreview.PlanToken == "" {
		t.Fatalf("exact reconciliation preview = %+v", exactPreview)
	}
	if selection, err := engine.Files().ValidateDuplicateReconciliation(context.Background(), user, exactPreview.PlanToken); err != nil || len(selection.Items) != 1 {
		t.Fatalf("exact reconciliation validation = %+v, %v", selection, err)
	}

	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/backup/project-a/unique.txt"), []byte("unique"))
	page, err = engine.Files().ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if countDuplicateGroups(page.Groups, domain.DuplicateDirectory) != 0 {
		t.Fatalf("stale exact directory duplicate remained: %+v", page.Groups)
	}
	comparison, err := engine.Files().CompareDuplicateDirectories(context.Background(), user, domain.DuplicateDirectoryComparisonRequest{
		Left:  domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")},
		Right: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Exact || comparison.CommonFiles != 1 || comparison.CommonBytes != 10 || comparison.LeftOnlyFiles != 0 || comparison.RightOnlyFiles != 1 || comparison.RightOnlyBytes != 6 {
		t.Fatalf("directory comparison = %+v", comparison)
	}
	overlaps, err = engine.Files().ListDuplicateDirectoryOverlaps(context.Background(), user, domain.DuplicateDirectoryOverlapRequest{
		Directory: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")}, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundPartialOverlap := false
	for _, candidate := range overlaps.Candidates {
		if candidate.Comparison.Right.Path.String() == "/backup/project-a" {
			foundPartialOverlap = !candidate.Comparison.Exact && candidate.Comparison.CommonFiles == 1 && candidate.Comparison.RightOnlyFiles == 1 && candidate.SharedSketch > 0 && candidate.SharedSketch < candidate.SketchSize
		}
	}
	if !foundPartialOverlap {
		t.Fatalf("partial overlap candidate is missing: %+v", overlaps.Candidates)
	}
	partialPreview, err := engine.Files().PreviewDuplicateReconciliation(context.Background(), user, domain.DuplicateReconciliationPreviewRequest{
		Left:       domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")},
		Right:      domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")},
		RemoveFrom: domain.DuplicateSideRight, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if partialPreview.Comparison.Exact || len(partialPreview.Items) != 1 || partialPreview.Items[0].Remove.Path.String() != "/backup/project-a/same.txt" || partialPreview.Items[0].Keep.Path.String() != "/project-a/same.txt" || partialPreview.ReclaimableBytes != 10 || partialPreview.PlanToken == "" {
		t.Fatalf("partial reconciliation preview = %+v", partialPreview)
	}
	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/backup/project-a/later.txt"), []byte("later"))
	if _, err := engine.Files().ValidateDuplicateReconciliation(context.Background(), user, partialPreview.PlanToken); err == nil {
		t.Fatal("stale reconciliation plan was accepted after its directory changed")
	}

	fileGroup = duplicateGroupByKind(t, page.Groups, domain.DuplicateFile)
	ignored, err := engine.Files().SetDuplicateGroupIgnored(context.Background(), user, domain.SetDuplicateIgnoredRequest{GroupID: fileGroup.ID, Ignored: true})
	if err != nil || !ignored.Ignored || ignored.Revision != 1 {
		t.Fatalf("ignore result = %+v, %v", ignored, err)
	}
	visible, err := engine.Files().ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if countDuplicateGroups(visible.Groups, domain.DuplicateFile) != 0 {
		t.Fatalf("ignored file group remained visible: %+v", visible.Groups)
	}
	withIgnored, err := engine.Files().ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Limit: 20, IncludeIgnored: true})
	if err != nil {
		t.Fatal(err)
	}
	fileGroup = duplicateGroupByKind(t, withIgnored.Groups, domain.DuplicateFile)
	if !fileGroup.Ignored || fileGroup.IgnoreRevision != ignored.Revision {
		t.Fatalf("included ignored group = %+v", fileGroup)
	}
	ignoredPreview, err := engine.Files().PreviewDuplicateReconciliation(context.Background(), user, domain.DuplicateReconciliationPreviewRequest{
		Left:       domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")},
		Right:      domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")},
		RemoveFrom: domain.DuplicateSideRight,
	})
	if err != nil || len(ignoredPreview.Items) != 0 || ignoredPreview.PlanToken != "" {
		t.Fatalf("ignored reconciliation preview = %+v, %v", ignoredPreview, err)
	}
}

func TestDuplicateCatalogFollowsSubtreeRootsWithoutWalkingDescendants(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2048, 2, 3, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(143, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 144, nil)
	user, _ := domain.ParseUserID("aWlpaWlpaWlpaWlpaWlpaQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/source", "/source/nested"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/source/nested/value.bin"), []byte("catalog"))
	if _, err := engine.Files().Copy(context.Background(), scope, scope, domain.CopyRequest{Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "duplicate-copy-0001"}); err != nil {
		t.Fatal(err)
	}
	groups, err := engine.Files().ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if countDuplicateGroups(groups.Groups, domain.DuplicateFile) != 0 || countDuplicateGroups(groups.Groups, domain.DuplicateDirectory) != 1 || duplicateGroupByKind(t, groups.Groups, domain.DuplicateDirectory).OccurrenceCount != 2 {
		t.Fatalf("copied subtree groups = %+v", groups.Groups)
	}
	comparison, err := engine.Files().CompareDuplicateDirectories(context.Background(), user, domain.DuplicateDirectoryComparisonRequest{
		Left: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/source")}, Right: domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/copy")},
	})
	if err != nil || !comparison.Exact || comparison.CommonFiles != 1 {
		t.Fatalf("copied immutable subtree comparison = %+v, %v", comparison, err)
	}
	if _, err := engine.Files().Move(context.Background(), scope, scope, domain.MoveRequest{Source: domain.MustParseUserPath("/copy"), Destination: domain.MustParseUserPath("/moved"), IdempotencyKey: "duplicate-move-0001"}); err != nil {
		t.Fatal(err)
	}
	directoryGroup := duplicateGroupByKind(t, groups.Groups, domain.DuplicateDirectory)
	occurrences, err := engine.Files().ListDuplicateOccurrences(context.Background(), user, domain.DuplicateOccurrenceRequest{GroupID: directoryGroup.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicateOccurrencePathsEqual(occurrences.Occurrences, "/moved", "/source") {
		t.Fatalf("moved occurrences = %+v", occurrences.Occurrences)
	}
	if _, err := engine.Files().Delete(context.Background(), scope, domain.DeleteRequest{Path: domain.MustParseUserPath("/moved"), IdempotencyKey: "duplicate-delete-01"}); err != nil {
		t.Fatal(err)
	}
	groups, err = engine.Files().ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 0 {
		t.Fatalf("deleted subtree left duplicates: %+v", groups.Groups)
	}
}

func TestDuplicateCatalogUsesBoundedAuthenticatedPagination(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2048, 2, 4, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(145, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 146, nil)
	user, _ := domain.ParseUserID("ampqampqampqampqampqag")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, item := range []struct {
		path string
		body string
	}{
		{"/a-1", "alpha"}, {"/a-2", "alpha"}, {"/a-3", "alpha"},
		{"/b-1", "bravo"}, {"/b-2", "bravo"},
	} {
		uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath(item.path), []byte(item.body))
	}
	var groups []domain.DuplicateGroup
	cursor := ""
	for {
		page, err := engine.Files().ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Kind: domain.DuplicateFile, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, page.Groups...)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(groups) != 2 || groups[0].ID == groups[1].ID {
		t.Fatalf("paged groups = %+v", groups)
	}
	group := groups[0]
	var occurrences []domain.DuplicateOccurrence
	cursor = ""
	for {
		page, err := engine.Files().ListDuplicateOccurrences(context.Background(), user, domain.DuplicateOccurrenceRequest{GroupID: group.ID, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		occurrences = append(occurrences, page.Occurrences...)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if int64(len(occurrences)) != group.OccurrenceCount {
		t.Fatalf("paged occurrences = %d; want %d", len(occurrences), group.OccurrenceCount)
	}
	other, _ := domain.ParseUserID("a2tra2tra2tra2tra2traw")
	if _, err := engine.Files().ListDuplicateGroups(context.Background(), other, domain.DuplicateGroupRequest{Kind: domain.DuplicateFile, Limit: 1, Cursor: groupsCursorForTest(t, engine.Files(), user)}); err == nil {
		t.Fatal("cross-owner duplicate cursor was accepted")
	}
}

func TestDuplicateReconciliationPagesPersistentDirectoryContentIndexes(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2048, 2, 5, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(147, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 148, nil)
	user, _ := domain.ParseUserID("bG1ubG1ubG1ubG1ubG1ubQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, directory := range []string{"/left", "/right"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(directory)}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
			uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath(directory+"/"+name), []byte("shared"))
		}
	}
	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/left/left-only.bin"), []byte("left only"))
	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/right/right-only.bin"), []byte("right only"))
	request := domain.DuplicateReconciliationPreviewRequest{
		Left:       domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/left")},
		Right:      domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/right")},
		RemoveFrom: domain.DuplicateSideRight, Limit: 1,
	}
	var removals []string
	for {
		page, err := engine.Files().PreviewDuplicateReconciliation(context.Background(), user, request)
		if err != nil {
			t.Fatal(err)
		}
		if page.Comparison.Exact || page.Comparison.CommonFiles != 3 || len(page.Items) != 1 || page.PlanToken == "" {
			t.Fatalf("paged reconciliation = %+v", page)
		}
		removals = append(removals, page.Items[0].Remove.Path.String())
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	if len(removals) != 3 || removals[0] != "/right/a.bin" || removals[1] != "/right/b.bin" || removals[2] != "/right/c.bin" {
		t.Fatalf("paged removals = %v", removals)
	}
}

func groupsCursorForTest(t *testing.T, files *portable.FileStore, user domain.UserID) string {
	t.Helper()
	page, err := files.ListDuplicateGroups(context.Background(), user, domain.DuplicateGroupRequest{Kind: domain.DuplicateFile, Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("duplicate cursor setup = %+v, %v", page, err)
	}
	return page.NextCursor
}

func duplicateGroupByKind(t *testing.T, groups []domain.DuplicateGroup, kind domain.DuplicateKind) domain.DuplicateGroup {
	t.Helper()
	for _, group := range groups {
		if group.Kind == kind {
			return group
		}
	}
	t.Fatalf("duplicate group %q not found in %+v", kind, groups)
	return domain.DuplicateGroup{}
}

func countDuplicateGroups(groups []domain.DuplicateGroup, kind domain.DuplicateKind) int {
	count := 0
	for _, group := range groups {
		if group.Kind == kind {
			count++
		}
	}
	return count
}

func duplicateOccurrencePathsEqual(occurrences []domain.DuplicateOccurrence, paths ...string) bool {
	if len(occurrences) != len(paths) {
		return false
	}
	want := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		want[path] = struct{}{}
	}
	for _, occurrence := range occurrences {
		if _, found := want[occurrence.Path.String()]; !found {
			return false
		}
		delete(want, occurrence.Path.String())
	}
	return len(want) == 0
}

func duplicateOverlapContainsPath(candidates []domain.DuplicateDirectoryOverlapCandidate, path string) bool {
	_, found := duplicateOverlapByPath(candidates, path)
	return found
}

func duplicateOverlapByPath(candidates []domain.DuplicateDirectoryOverlapCandidate, path string) (domain.DuplicateDirectoryOverlapCandidate, bool) {
	for _, candidate := range candidates {
		if candidate.Comparison.Right.Path.String() == path {
			return candidate, true
		}
	}
	return domain.DuplicateDirectoryOverlapCandidate{}, false
}
