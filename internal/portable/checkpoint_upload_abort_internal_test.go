package portable

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestCheckpointUploadAbortClosureValidatesAuthorityAndMembership(t *testing.T) {
	for _, test := range []struct {
		name         string
		mutate       func(*storageformat.PortableUploadBatchAbort)
		mutateMember func(*storageformat.PortableUploadRecord)
		deleteMember bool
		malformed    bool
		want         error
	}{
		{name: "valid"},
		{name: "owner", mutate: func(a *storageformat.PortableUploadBatchAbort) { a.OwnerID += "x" }, want: domain.ErrInvalid},
		{name: "batch", mutate: func(a *storageformat.PortableUploadBatchAbort) {
			a.BatchID = storageformat.Digest([]byte("another-batch"))
		}, want: domain.ErrInvalid},
		{name: "missing-member", mutate: func(a *storageformat.PortableUploadBatchAbort) { a.Count = 3 }, want: domain.ErrInvalid},
		{name: "member-count", mutate: func(a *storageformat.PortableUploadBatchAbort) { a.Count = 1; a.Aborted = []byte{1} }, want: domain.ErrInvalid},
		{name: "bitmap", mutate: func(a *storageformat.PortableUploadBatchAbort) { a.Aborted = []byte{255} }, want: domain.ErrInvalid},
		{name: "deleted-member", deleteMember: true, want: domain.ErrInvalid},
		{name: "malformed-abort", malformed: true, want: domain.ErrInvalid},
		{name: "member-batch", mutateMember: func(r *storageformat.PortableUploadRecord) {
			r.Batch.BatchID = storageformat.Digest([]byte("other-membership"))
		}, want: domain.ErrInvalid},
		{name: "member-index", mutateMember: func(r *storageformat.PortableUploadRecord) { r.Batch.Index = 1 }, want: domain.ErrInvalid},
		{name: "member-no-batch", mutateMember: func(r *storageformat.PortableUploadRecord) { r.Batch = nil }, want: domain.ErrInvalid},
		{name: "completed-aborted-member", mutateMember: func(r *storageformat.PortableUploadRecord) {
			r.State = storageformat.UploadCompleted
			r.Completion = &storageformat.PortableUploadCompletion{MD5: "1B2M2Y8AsgTpgAmY7PhCfg", CRC32C: "AAAAAA", ModifiedAt: r.CreatedAt}
		}, want: domain.ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newUploadBatchScaleFixture(t, 0x19274386)
			caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("closure-abort", 2))
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, domain.AbortUploadBatchRequest{
				UploadIDs: []domain.UploadID{caps[0].UploadID, caps[1].UploadID}, BatchID: caps[0].BatchID, IdempotencyKey: "closure-cancel",
			}); err != nil {
				t.Fatal(err)
			}
			ref := uploadDomainReference(fixture.scope.UserID())
			store := fixture.engine.stateDomainStore()
			if test.mutate != nil || test.malformed {
				value, err := store.get(ctx, ref, uploadBatchAbortKey(caps[0].BatchID))
				if err != nil {
					t.Fatalf("abort authority: %v", err)
				}
				var abort storageformat.PortableUploadBatchAbort
				if err := decodeCanonicalValue(value.Data, &abort); err != nil {
					t.Fatal(err)
				}
				if test.mutate != nil {
					test.mutate(&abort)
				}
				body, err := storageformat.EncodeCanonical(abort)
				if err != nil {
					t.Fatal(err)
				}
				if test.malformed {
					body = []byte("{")
				}
				_, err = store.mutate(ctx, ref, consistencyDomainMutation{ID: "corrupt-abort-" + test.name, Changes: []consistencyDomainChange{{Key: uploadBatchAbortKey(caps[0].BatchID), Require: domainValuePresent, Value: body}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.mutateMember != nil || test.deleteMember {
				key := uploadRecordKey(string(caps[0].UploadID))
				value, err := store.get(ctx, ref, key)
				if err != nil {
					t.Fatal(err)
				}
				var record storageformat.PortableUploadRecord
				if err := decodeCanonicalValue(value.Data, &record); err != nil {
					t.Fatal(err)
				}
				if test.mutateMember != nil {
					test.mutateMember(&record)
				}
				body, err := storageformat.EncodeCanonical(record)
				if err != nil {
					t.Fatal(err)
				}
				if test.deleteMember {
					body = nil
				}
				_, err = store.mutate(ctx, ref, consistencyDomainMutation{ID: "corrupt-member-" + test.name, Changes: []consistencyDomainChange{{Key: key, Require: domainValuePresent, Value: body, Delete: test.deleteMember}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			head, err := store.loadHead(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.engine.validateKnownControlDomainValuesForSchema(ctx, ref, head.head, true); !errors.Is(err, test.want) {
				t.Fatalf("abort closure = %v; want %v", err, test.want)
			}
		})
	}
}
