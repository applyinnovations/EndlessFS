package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestUploadBatchNewIntentAfterCancellationHasIndependentIdentity(t *testing.T) {
	ctx := context.Background()
	fixture := newUploadBatchScaleFixture(t, 0x19736542)
	requests := scaleUploadRequestsCount("same-destination", 2)
	first, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]domain.UploadID, len(first))
	for index, capability := range first {
		ids[index] = capability.UploadID
	}
	if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, domain.AbortUploadBatchRequest{
		UploadIDs: ids, BatchID: first[0].BatchID, IdempotencyKey: "cancel-first-intent",
	}); err != nil {
		t.Fatal(err)
	}
	for index := range requests {
		requests[index].IdempotencyKey += "-new-intent"
	}
	second, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil {
		t.Fatalf("fresh upload intent at cancelled destinations: %v", err)
	}
	for index, capability := range second {
		if capability.UploadID == first[index].UploadID || capability.BatchID == first[index].BatchID {
			t.Fatal("independent upload intent reused cancelled authority")
		}
	}
	replayed, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil {
		t.Fatalf("replay fresh intent: %v", err)
	}
	for index := range replayed {
		if replayed[index].UploadID != second[index].UploadID || replayed[index].BatchID != second[index].BatchID {
			t.Fatal("replay changed published identity")
		}
	}
	if _, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests[:1]); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("subset replay must reject changed membership: %v", err)
	}
	requests[0], requests[1] = requests[1], requests[0]
	if _, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("reordered replay must reject changed membership: %v", err)
	}
}

func TestUploadBatchPredecessorInterruptedIntentRetainsPublishedIdentity(t *testing.T) {
	body, err := os.ReadFile("testdata/upload-identity/v0.7.5-after-intents.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != "c6b4d5d73a94073a6d761889b83cd56be46363de37157c24cb0cb50e2b300d84" {
		t.Fatalf("immutable predecessor fixture changed: %s", got)
	}
	var fixture struct {
		ProducerCommit string            `json:"producerCommit"`
		Phase          string            `json:"phase"`
		BatchID        string            `json:"batchID"`
		UploadIDs      []string          `json:"uploadIDs"`
		StateObjects   map[string][]byte `json:"stateObjects"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.ProducerCommit != "facf91f1ff64970c16717692619bfd2a93f5d1e9" || fixture.Phase != StepUploadBatchAfterIntents || len(fixture.UploadIDs) != 2 {
		t.Fatal("invalid predecessor fixture provenance")
	}
	ctx := context.Background()
	stateBackend, fileBackend := objectmemory.New(), objectmemory.New()
	for name, body := range fixture.StateObjects {
		key, err := objectstore.ParseKey(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stateBackend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	clock := domain.NewFixedClock(time.Date(2068, 1, 2, 3, 4, 5, 0, time.UTC))
	server := httptest.NewServer(fileBackend)
	t.Cleanup(server.Close)
	if err := fileBackend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(&deterministicScaleReader{state: 0x54321976})); err != nil {
		t.Fatal(err)
	}
	options := Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs:      domain.NewIDGenerator(&deterministicScaleReader{state: 0x19736542}),
		Writer:   WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32),
	}
	if _, err := Open(ctx, options); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("upgrade must wait for predecessor capability drain: %v", err)
	}
	clock.Advance(20 * time.Minute)
	engine, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	superblockObject, err := stateBackend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		t.Fatal(err)
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(superblockObject.Body, &superblock); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(superblock.RequiredFeatures, "upload-intent-identities-v1") {
		t.Fatal("identity repair did not cross a writer-compatibility boundary")
	}
	owner, err := domain.ParseUserID("U0NBTEVTQ0FMRVNDQUxFU0NBTA")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	requests := scaleUploadRequestsCount("identity-predecessor", 2)
	for index, request := range requests {
		_, fingerprint, err := normalizePortableUploadRequest(scope, request)
		if err != nil {
			t.Fatal(err)
		}
		record, found, err := engine.Files().portableUploadByIdempotency(ctx, owner, request.IdempotencyKey, fingerprint)
		if err != nil || !found || record.Batch == nil {
			t.Fatalf("read predecessor authority: %+v, %v", record, err)
		}
		if record.Batch.BatchID != fixture.BatchID || record.UploadID != fixture.UploadIDs[index] {
			t.Fatal("predecessor admission was rebound to new identifiers")
		}
		status, err := engine.Files().UploadStatus(ctx, scope, domain.UploadID(fixture.UploadIDs[index]))
		if err != nil || status.State != domain.UploadStateExpired {
			t.Fatalf("drained predecessor status: %+v, %v", status, err)
		}
	}
	if _, err := engine.Files().CreateUploadBatch(ctx, scope, requests); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired predecessor must not create a new capability: %v", err)
	}
	for index := range requests {
		requests[index].IdempotencyKey += "-fresh"
	}
	fresh, err := engine.Files().CreateUploadBatch(ctx, scope, requests)
	if err != nil {
		t.Fatalf("fresh intent after predecessor expiry: %v", err)
	}
	for index, capability := range fresh {
		if capability.BatchID == fixture.BatchID || string(capability.UploadID) == fixture.UploadIDs[index] {
			t.Fatal("fresh intent reused predecessor authority")
		}
	}
}

func TestUploadBatchIndependentIntentsConvergeAcrossReplicas(t *testing.T) {
	for _, replicas := range []int{2, 4, 8} {
		t.Run(fmt.Sprint(replicas), func(t *testing.T) {
			ctx := context.Background()
			fixture := newUploadBatchScaleFixture(t, 0x86432197)
			engines := []*Engine{fixture.engine}
			requests := make([][]domain.CreateUploadRequest, replicas)
			capabilities := make([][]domain.UploadCapability, replicas)
			for index := range requests {
				requests[index] = scaleUploadRequestsCount("shared-target", 2)
				for item := range requests[index] {
					requests[index][item].IdempotencyKey += fmt.Sprintf("-replica-%d", index)
				}
				if index == 0 {
					continue
				}
				engine, err := Open(ctx, Options{
					Backend: fixture.engine.backend, FileBackend: fixture.engine.fileBackend,
					Clock: fixture.engine.clock, IDs: domain.NewIDGenerator(&deterministicScaleReader{state: uint64(index + 1)}),
					Writer:   WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}},
					LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32),
				})
				if err != nil {
					t.Fatal(err)
				}
				engines = append(engines, engine)
			}
			interleaved := false
			fixture.engine.scheduler = SchedulerFunc(func(ctx context.Context, step string) error {
				if step != StepDomainBeforeHeadCommit || interleaved {
					return nil
				}
				interleaved = true
				for index := 1; index < replicas; index++ {
					var err error
					capabilities[index], err = engines[index].Files().CreateUploadBatch(ctx, fixture.scope, requests[index])
					if err != nil {
						return err
					}
				}
				return nil
			})
			var err error
			capabilities[0], err = fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests[0])
			if err != nil || !interleaved {
				t.Fatalf("interleaved admission: %v, scheduled=%v", err, interleaved)
			}
			seen := make(map[domain.UploadID]bool)
			for _, batch := range capabilities {
				for _, capability := range batch {
					if seen[capability.UploadID] {
						t.Fatal("independent replica intents shared upload authority")
					}
					seen[capability.UploadID] = true
				}
			}
			ids := []domain.UploadID{capabilities[0][0].UploadID, capabilities[0][1].UploadID}
			if err := engines[0].Files().AbortUploadBatch(ctx, fixture.scope, domain.AbortUploadBatchRequest{
				UploadIDs: ids, BatchID: capabilities[0][0].BatchID, IdempotencyKey: "cancel-one-replica",
			}); err != nil {
				t.Fatal(err)
			}
			for index := 1; index < replicas; index++ {
				for _, capability := range capabilities[index] {
					status, err := engines[index].Files().UploadStatus(ctx, fixture.scope, capability.UploadID)
					if err != nil || status.State != domain.UploadStateActive {
						t.Fatalf("another intent was affected by cancellation: %+v, %v", status, err)
					}
				}
			}
		})
	}
}
