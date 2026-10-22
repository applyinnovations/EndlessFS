package e2e

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestBrowserFixtureSchedulingModesAreClosedAndEvidenceSerial(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{{"", 2}, {"1", 1}, {"2", 2}, {"4", 4}} {
		got, err := resolveE2ESchedule([]string{e2eParallelismEnvironment + "=" + test.value}, false)
		if err != nil || got.limit != test.want || got.forcedSerial {
			t.Fatalf("closed mode %q: %+v %v", test.value, got, err)
		}
	}
	// Normal atomic coverage export is output-only in every supported mode.
	for _, test := range []struct {
		value string
		want  int
	}{{"", 2}, {"1", 1}, {"2", 2}, {"4", 4}} {
		got, err := resolveE2ESchedule([]string{e2eParallelismEnvironment + "=" + test.value, "ENDLESSFS_COVERAGE_PROFILE=owned-output"}, false)
		if err != nil || got.limit != test.want || got.forcedSerial {
			t.Fatalf("coverage export changed mode %q: %+v %v", test.value, got, err)
		}
	}
	for _, value := range []string{"0", "3", "8", "auto", " 2", "2 ", "-1"} {
		if _, err := resolveE2ESchedule([]string{e2eParallelismEnvironment + "=" + value}, false); err == nil {
			t.Fatalf("unsupported scheduling value %q accepted", value)
		}
	}
	for _, name := range []string{"ENDLESSFS_SOURCE_QUALIFICATION_DIRECTORY", "ENDLESSFS_LAYOUT_SCREENSHOT_PREFIX",
		"ENDLESSFS_UPLOAD_PIPELINE_SCREENSHOT", "ENDLESSFS_SMART_RECOVERY_EVIDENCE",
		"ENDLESSFS_CPU_PROFILE", "ENDLESSFS_TRACE_PROFILE", "ENDLESSFS_COVERAGE_PROFILE_EXTRA",
		"ENDLESSFS_UI_BENCHMARK_OUTPUT", "ENDLESSFS_FUTURE_EVIDENCE_OUTPUT"} {
		got, err := resolveE2ESchedule([]string{e2eParallelismEnvironment + "=4", name + "=owned-output"}, false)
		if err != nil || got.limit != 1 || !got.forcedSerial {
			t.Fatalf("evidence selector did not serialize: %s", name)
		}
	}
	got, err := resolveE2ESchedule([]string{e2eParallelismEnvironment + "=4", "ENDLESSFS_COVERAGE_PROFILE=owned-output"}, true)
	if err != nil || got.limit != 1 || !got.forcedSerial {
		t.Fatal("optional profiling did not serialize")
	}
	if _, err := resolveE2ESchedule([]string{e2eParallelismEnvironment + "=invalid", "ENDLESSFS_SMART_RECOVERY_EVIDENCE=output"}, false); err == nil {
		t.Fatal("forced serial concealed malformed scheduling mode")
	}
}

func TestBrowserFixturePoolRetainsLeaseUntilCleanupAndCancelsWaiter(t *testing.T) {
	pool := newE2EFixturePool(2)
	ctx := context.Background()
	if err := pool.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := pool.acquire(canceled); err != context.Canceled {
		t.Fatal("canceled waiter acquired fixture lease")
	}
	if pool.active.Load() != 2 || pool.maximum.Load() != 2 {
		t.Fatal("canceled wait altered lease count")
	}
	pool.release()
	if err := pool.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	pool.release()
	pool.release()
	if pool.active.Load() != 0 || len(pool.slots) != 0 {
		t.Fatal("fixture lease leaked")
	}
	order := []string{}
	var mu sync.Mutex
	t.Run("cleanup-order", func(t *testing.T) {
		testContext := t.Context()
		if err := pool.acquire(testContext); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if testContext.Err() != context.Canceled {
				t.Error("test context was not canceled before final lease cleanup")
			}
			pool.release()
			mu.Lock()
			order = append(order, "release")
			mu.Unlock()
		})
		t.Cleanup(func() {
			if pool.active.Load() != 1 {
				t.Error("slot released before fixture cleanup")
			}
			mu.Lock()
			order = append(order, "fixture")
			mu.Unlock()
		})
	})
	if !reflect.DeepEqual(order, []string{"fixture", "release"}) || pool.active.Load() != 0 {
		t.Fatal("fixture release was not last")
	}
}

// Done is evaluated only after acquire has passed its initial Err check.
// With every slot held, this proves the waiter reached its blocking select.
type e2eBlockedFixtureContext struct {
	context.Context
	selectReached chan struct{}
	once          sync.Once
}

func (ctx *e2eBlockedFixtureContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.selectReached) })
	return ctx.Context.Done()
}

func TestBrowserFixturePoolBlockedWaiterCancellationWakesWithoutLease(t *testing.T) {
	for _, limit := range []int{1, 2, 4} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			pool := newE2EFixturePool(limit)
			for range limit {
				if err := pool.acquire(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				for range limit {
					pool.release()
				}
			}()
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			waiting := &e2eBlockedFixtureContext{Context: base, selectReached: make(chan struct{})}
			finished := make(chan error, 1)
			go func() { finished <- pool.acquire(waiting) }()
			<-waiting.selectReached
			select {
			case <-finished:
				t.Fatal("full pool did not block its additional waiter")
			default:
			}
			cancel()
			if err := <-finished; err != context.Canceled {
				t.Fatal("blocked waiter did not wake with cancellation")
			}
			if pool.active.Load() != int32(limit) || len(pool.slots) != limit || pool.maximum.Load() != int32(limit) {
				t.Fatal("canceled blocked waiter altered held leases or high-water count")
			}
		})
	}
}

func TestBrowserFixturePoolConcurrentHighWaterIsExactlyClosedLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 4} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			pool := newE2EFixturePool(limit)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			workers := limit * 4
			start := make(chan struct{})
			entered := make(chan int, workers)
			finished := make(chan error, workers)
			releases := make([]chan struct{}, workers)
			for worker := range workers {
				releases[worker] = make(chan struct{})
				go func(worker int) {
					<-start
					if err := pool.acquire(ctx); err != nil {
						finished <- err
						return
					}
					entered <- worker
					var err error
					select {
					case <-releases[worker]:
					case <-ctx.Done():
						err = ctx.Err()
					}
					pool.release()
					finished <- err
				}(worker)
			}
			close(start)
			for range workers / limit {
				held := make([]int, limit)
				for index := range held {
					held[index] = <-entered
				}
				if pool.active.Load() != int32(limit) || pool.maximum.Load() != int32(limit) || len(pool.slots) != limit {
					t.Fatal("concurrent holders did not attain exactly their fixture limit")
				}
				select {
				case <-entered:
					t.Fatal("additional worker entered a full fixture pool")
				default:
				}
				for _, worker := range held {
					close(releases[worker])
				}
			}
			for range workers {
				if err := <-finished; err != nil {
					t.Fatal(err)
				}
			}
			if pool.active.Load() != 0 || len(pool.slots) != 0 || pool.maximum.Load() != int32(limit) {
				t.Fatal("concurrent fixture pool leaked or exceeded its closed limit")
			}
		})
	}
}

// This inventory binds every browser entry point. Adding/removing a
// test is a deliberate inventory update, never a silent selection change.
var scheduledE2EInventory = []string{
	"TestE2EAutomaticRetryResumesOriginalBatchSession",
	"TestE2EBrowserBootstrapLoginDriveShareAndTrash",
	"TestE2EClearFailedHistoryPreservesMixedGroupsAndOtherOwners",
	"TestE2EClearFailedUploadAfterLostCompletionPreservesPublishedFile",
	"TestE2EClearFailedUploadsCleansSessionsAndPreservesCompletedFiles",
	"TestE2EInviteSettingsAdminRecoveryAndShareRevocation",
	"TestE2ELatestUploadProgressExcludesFailedHistory",
	"TestE2ELostCompletionResponseRetainsOrderAcrossReload",
	"TestE2ELostUploadResponseFinalizesEmptyAndChunkedObjectsOnce",
	"TestE2EMediaBrowserIsAvailableWithoutGeneratedPreviews",
	"TestE2EProductionFolderDropRetainsSourcesAcrossUploadOptions",
	"TestE2EQueuedUploadAdmissionPreservesCancellationAndReleasesTurn",
	"TestE2ERestoringTenThousandTransfersDoesNotBlockWorkspaceOrPollUploads",
	"TestE2ERetryFailedFinalizationAfterReloadDoesNotRequireSource",
	"TestE2ESmartMergeCancellationReleasesHeldFingerprintWork",
	"TestE2ESmartMergePartitionsUTF8RequestsWithinControlBodyLimit",
	"TestE2ESmartMergeUploadsAndPublishesWhileAnotherFingerprintIsHeld",
	"TestE2ESmartRecoveryPersistsPartialUploadAcrossRefreshAndLogin",
	"TestE2ESmartRecoveryProductionCardinalityWarmTransfersOnlyRemainingFiles",
	"TestE2ESmartRecoveryResumesOnlyUnconfirmedBytesAfterRefreshLogin",
	"TestE2EThumbnailQueueProgressesAcrossRerenderWithoutRefresh",
	"TestE2EUploadActionsRetryOnlyRejectedFinalizationWithoutResendingData",
	"TestE2EUploadAdmissionIgnoresReplyForReplacedAttempt",
	"TestE2EUploadAdmissionWaitsForNamespaceTurnWithoutBlockingData",
	"TestE2EUploadDirectoryPreparationCancellationReleasesNamespaceQueue",
	"TestE2EUploadDirectoryPreparationPagesOverlapIndependentSiblings",
	"TestE2EUploadDirectoryPreparationReplaysOneBoundedPage",
	"TestE2EUploadETAUsesQualifiedCohortRatesAndStableDisplayCadence",
	"TestE2EUploadExpiredRetryWaitsForDurableAdmissionKey",
	"TestE2EUploadExpiredRetryWaitsForDurableIdentity",
	"TestE2EUploadIndependentAdmissionCoalescesPageRetries",
	"TestE2EUploadIndependentAdmissionExplicitReplanRenewsOnlyRejectedAttempt",
	"TestE2EUploadIndependentAdmissionMultiplePagesPreserveEveryItem",
	"TestE2EUploadIndependentAdmissionReloadsAfterLostReply",
	"TestE2EUploadIndependentAdmissionRetriesOnlyFailedItem",
	"TestE2EUploadIndependentAdmissionRetriesRollingRouteGap",
	"TestE2EUploadLayoutKeepsColumnsErrorsAndActionsReadable",
	"TestE2EUploadLegacyAdmissionLedgerRetainsExactEnvelope",
	"TestE2EUploadLoopbackConnectivitySignalPausesAndResumesSmartPlanning",
	"TestE2EUploadPlannerHashesMD5AndCRC32CInOneWorkerPass",
	"TestE2EUploadPreparingCreditsCompletedSiblingsAndReconnectsExpiredPage",
	"TestE2EUploadRecoveryCancelledItemCannotBeResurrectedBySharedPage",
	"TestE2EUploadRecoveryRejectsChangedReconnectedSourceBeforeData",
	"TestE2EUploadRecoveryRejectsMisboundNestedIDsBeforeProgressOrData",
	"TestE2EUploadRecoveryRequestBudgetAndBoundedRendering",
	"TestE2EUploadResponseCapacityCancelsDeferredReplyForReplacedAttempt",
	"TestE2EUploadResponseCapacityKeepsAdmissionAndRecoversOnlyDeferred",
	"TestE2EUploadResponseCapacityTerminalAttemptCanBeCleared",
	"TestE2EUploadSourceBackpressureRemainsResponsiveAndCompletes",
	"TestE2EUploadSourceBufferBudgetAndCancellation",
	"TestE2EUploadSourceHardwareQualification",
	"TestE2EUploadSourceReadFailureReleasesBudgetAndRequestsReconnection",
	"TestE2EUploadStalledSourceDoesNotBlockVerifiedFiles",
	"TestE2EUploadStoredSourceRestoreClearsObsoleteSourceRequiredBeforeSmartPlanning",
	"TestE2EUploadVerifiedFilesPublishWhileOtherAcknowledgementsContinue",
	"TestE2EUpstreamGCSWorkerPoolUploadsOneHundredFilesAndCoalescesRefresh",
}

func TestBrowserFixturePolicyCoversEveryOriginalEntryAndNoFactories(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source root unavailable")
	}
	root := filepath.Dir(file)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = parsed
	}
	got, problems := e2eSchedulingPolicy(files)
	for _, problem := range problems {
		t.Error(problem)
	}
	if !reflect.DeepEqual(got, scheduledE2EInventory) {
		t.Fatalf("browser test inventory changed: count=%d want=%d", len(got), len(scheduledE2EInventory))
	}
}

func e2eSchedulingPolicy(files map[string]*ast.File) ([]string, []string) {
	names, problems := []string{}, []string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "parallelE2EFixture" && !hasDisabledE2EReturn(fn.Body) {
				problems = append(problems, "disabled scheduling must return before configuration without skipping the original body")
			}
			browser := strings.HasPrefix(fn.Name.Name, "TestE2E")
			var allowed *ast.CallExpr
			if browser {
				names = append(names, fn.Name.Name)
				if len(fn.Body.List) == 0 || !isE2ELeaseCall(fn.Body.List[0]) {
					problems = append(problems, fn.Name.Name+": fixture lease must be first")
				} else {
					allowed = fn.Body.List[0].(*ast.ExprStmt).X.(*ast.CallExpr)
				}
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "parallelE2EFixture" && (!browser || call != allowed) {
					problems = append(problems, fn.Name.Name+": nested/factory fixture lease forbidden")
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Parallel" && fn.Name.Name != "parallelE2EFixture" {
					problems = append(problems, fn.Name.Name+": parallelism only in top-level scheduler")
				}
				return true
			})
		}
	}
	sort.Strings(names)
	sort.Strings(problems)
	return names, problems
}

// The scheduler must leave original disabled/explicit-hardware contracts to
// the unchanged test body. Its second statement must be this exact guard.
func hasDisabledE2EReturn(body *ast.BlockStmt) bool {
	if body == nil || len(body.List) < 2 {
		return false
	}
	first, ok := body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	helper, ok := first.X.(*ast.CallExpr)
	if !ok || len(helper.Args) != 0 {
		return false
	}
	method, ok := helper.Fun.(*ast.SelectorExpr)
	if !ok || method.Sel.Name != "Helper" {
		return false
	}
	receiver, ok := method.X.(*ast.Ident)
	if !ok || receiver.Name != "t" {
		return false
	}
	guard, ok := body.List[1].(*ast.IfStmt)
	if !ok || guard.Init != nil || guard.Else != nil || len(guard.Body.List) != 1 {
		return false
	}
	condition, ok := guard.Cond.(*ast.BinaryExpr)
	if !ok || condition.Op != token.NEQ {
		return false
	}
	call, ok := condition.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	function, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || function.Sel.Name != "Getenv" {
		return false
	}
	packageName, ok := function.X.(*ast.Ident)
	if !ok || packageName.Name != "os" {
		return false
	}
	name, ok := call.Args[0].(*ast.BasicLit)
	if !ok || name.Kind != token.STRING || name.Value != "\"ENDLESSFS_RUN_E2E\"" {
		return false
	}
	expected, ok := condition.Y.(*ast.BasicLit)
	if !ok || expected.Kind != token.STRING || expected.Value != "\"1\"" {
		return false
	}
	exit, ok := guard.Body.List[0].(*ast.ReturnStmt)
	return ok && len(exit.Results) == 0
}

func isE2ELeaseCall(statement ast.Stmt) bool {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	name, ok := call.Fun.(*ast.Ident)
	if !ok || name.Name != "parallelE2EFixture" {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == "t"
}

func TestBrowserFixturePolicyRejectsWrongPrefixNestedAndLateLease(t *testing.T) {
	for name, body := range map[string]string{
		"valid":                 "func TestE2EGood(t *testing.T){ parallelE2EFixture(t); fixture() }",
		"late":                  "func TestE2EBad(t *testing.T){ fixture(); parallelE2EFixture(t) }",
		"subtest":               "func requireSmartRecoveryE2E(t *testing.T){ parallelE2EFixture(t) }",
		"nested-E2E":            "func TestE2EBad(t *testing.T){ parallelE2EFixture(t); t.Run(\"sub\",func(t *testing.T){ parallelE2EFixture(t) }) }",
		"factory":               "func newTestBrowser(t *testing.T){ t.Parallel() }",
		"valid-disabled":        "func parallelE2EFixture(t *testing.T){ t.Helper(); if os.Getenv(\"ENDLESSFS_RUN_E2E\") != \"1\" { return }; configure() }",
		"disabled-skip":         "func parallelE2EFixture(t *testing.T){ t.Helper(); if os.Getenv(\"ENDLESSFS_RUN_E2E\") != \"1\" { t.Skip() }; configure() }",
		"disabled-late":         "func parallelE2EFixture(t *testing.T){ configure(); t.Helper(); if os.Getenv(\"ENDLESSFS_RUN_E2E\") != \"1\" { return } }",
		"disabled-config-first": "func parallelE2EFixture(t *testing.T){ configure(); if os.Getenv(\"ENDLESSFS_RUN_E2E\") != \"1\" { return } }",
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), "fixture_test.go", "package e2e\n"+body, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, problems := e2eSchedulingPolicy(map[string]*ast.File{"fixture": parsed})
		if strings.HasPrefix(name, "valid") != (len(problems) == 0) {
			t.Fatalf("policy case %s: %v", name, problems)
		}
	}
}
