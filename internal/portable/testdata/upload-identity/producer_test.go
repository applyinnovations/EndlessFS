package portable

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

func TestProducePredecessorUploadIdentityFixture(t *testing.T) {
	ctx := context.Background()
	fixture := newUploadBatchScaleFixture(t, 0x19736542)
	fixture.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepUploadBatchAfterIntents {
			return domain.ErrUnavailable
		}
		return nil
	})
	requests := scaleUploadRequestsCount("identity-predecessor", 2)
	if _, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupt predecessor admission: %v", err)
	}
	objects := make(map[string][]byte)
	cursor := ""
	for {
		page, err := fixture.engine.backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/v1/", Limit: 1000, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range page.Objects {
			object, err := fixture.engine.backend.Get(ctx, info.Key)
			if err != nil {
				t.Fatal(err)
			}
			objects[info.Key.String()] = object.Body
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	var expectedIDs []string
	var batchID string
	for _, request := range requests {
		_, fingerprint, err := normalizePortableUploadRequest(fixture.scope, request)
		if err != nil {
			t.Fatal(err)
		}
		record, found, err := fixture.engine.Files().portableUploadByIdempotency(ctx, fixture.scope.UserID(), request.IdempotencyKey, fingerprint)
		if err != nil || !found || record.Batch == nil {
			t.Fatalf("predecessor authority: %+v, %v", record, err)
		}
		expectedIDs = append(expectedIDs, record.UploadID)
		batchID = record.Batch.BatchID
	}
	body, err := json.MarshalIndent(struct {
		ProducerCommit string            `json:"producerCommit"`
		Phase          string            `json:"phase"`
		BatchID        string            `json:"batchID"`
		UploadIDs      []string          `json:"uploadIDs"`
		StateObjects   map[string][]byte `json:"stateObjects"`
	}{"facf91f1ff64970c16717692619bfd2a93f5d1e9", StepUploadBatchAfterIntents, batchID, expectedIDs, objects}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/private/tmp/endlessfs-upload-identity-predecessor.json", body, 0600); err != nil {
		t.Fatal(err)
	}
}
