package preview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func queueBoundaryFailure(base state.AtomicStore, boundary string) state.AtomicStore {
	return queueFaultStore{AtomicStore: base,
		get: func(ctx context.Context, key state.Key) (state.Value, error) {
			if boundary == "job-read" && strings.HasPrefix(key.String(), state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-job").String()) || boundary == "lane-read" && strings.HasPrefix(key.String(), state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-lane").String()) {
				return state.Value{}, domain.ErrUnavailable
			}
			return base.Get(ctx, key)
		},
		list: func(ctx context.Context, prefix state.Prefix, page state.PageRequest) (state.Page, error) {
			if boundary == "index-read" {
				return state.Page{}, domain.ErrUnavailable
			}
			return base.List(ctx, prefix, page)
		},
		mutate: func(ctx context.Context, m state.Mutation) (state.MutationOutcome, error) {
			if boundary == "write" {
				return state.MutationOutcome{}, domain.ErrUnavailable
			}
			if boundary == "conflict" {
				return state.MutationOutcome{}, domain.ErrPreconditionFailed
			}
			return base.Mutate(ctx, m)
		},
	}
}

func TestThumbnailQueueAdmissionFailuresRemainRetryable(t *testing.T) {
	for _, boundary := range []string{"job-read", "write", "conflict"} {
		t.Run(boundary, func(t *testing.T) {
			s, binding, item, _ := queueTestService(t)
			base := s.state
			s.state = queueBoundaryFailure(base, boundary)
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("admission failure=%v", err)
			}
			s.state = base
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatalf("could not retry durable admission: %v", err)
			}
			if _, found, err := s.claimThumbnail(t.Context()); err != nil || !found {
				t.Fatalf("retry lost requested job: %v %v", found, err)
			}
		})
	}
}

func TestThumbnailQueueClaimFailuresLeaveJobPending(t *testing.T) {
	for _, boundary := range []string{"job-read", "lane-read", "index-read", "write", "conflict"} {
		t.Run(boundary, func(t *testing.T) {
			s, binding, item, _ := queueTestService(t)
			base := s.state
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			s.state = queueBoundaryFailure(base, boundary)
			if _, found, err := s.claimThumbnail(t.Context()); found || boundary != "conflict" && !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("failed claim=%v %v", found, err)
			}
			s.state = base
			job, _, err := s.loadThumbnail(t.Context(), thumbnailID(binding))
			if err != nil || job.State != StateQueued || job.Epoch != 0 {
				t.Fatalf("failed claim consumed work: %+v %v", job, err)
			}
			if _, found, err := s.claimThumbnail(t.Context()); err != nil || !found {
				t.Fatalf("retry: %v %v", found, err)
			}
		})
	}
}

func TestThumbnailQueueCompletionFailuresPreserveClaim(t *testing.T) {
	for _, boundary := range []string{"job-read", "lane-read", "write", "conflict"} {
		t.Run(boundary, func(t *testing.T) {
			s, binding, item, _ := queueTestService(t)
			base := s.state
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			claim, found, err := s.claimThumbnail(t.Context())
			if err != nil || !found {
				t.Fatal(err)
			}
			s.state = queueBoundaryFailure(base, boundary)
			if err := s.finishThumbnail(t.Context(), claim, ItemResult{State: StateReady}, nil); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("completion failure=%v", err)
			}
			s.state = base
			job, _, err := s.loadThumbnail(t.Context(), thumbnailID(binding))
			if err != nil || job.LeaseID != claim.job.LeaseID {
				t.Fatalf("completion failure lost claim: %+v %v", job, err)
			}
			if err := s.finishThumbnail(t.Context(), claim, ItemResult{State: StateReady}, nil); err != nil {
				t.Fatalf("completion retry=%v", err)
			}
		})
	}
}

func TestThumbnailQueueStaleCompletionCannotReleaseReplacedLane(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "successor-lease", true: "corrupt-lane"}[corrupt], func(t *testing.T) {
			s, binding, item, clock := queueTestService(t)
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			claim, found, err := s.claimThumbnail(t.Context())
			if err != nil || !found {
				t.Fatalf("initial claim: %v %v", found, err)
			}
			laneValue, err := s.state.Get(t.Context(), claim.laneKey)
			if err != nil {
				t.Fatal(err)
			}
			successor, err := s.ids.OpaqueID()
			if err != nil {
				t.Fatal(err)
			}
			lane := thumbnailLane{SchemaVersion: 1, Claims: 2, LeaseID: successor, LeaseExpiresAt: clock.Now().Add(s.options.OperationTimeout)}
			if corrupt {
				lane.SchemaVersion = 99
			}
			body, _ := state.EncodeJSON(lane)
			if _, err := s.state.Mutate(t.Context(), state.Mutation{ID: "replace-thumbnail-lane", RetainUntil: clock.Now().Add(s.options.OperationRetention), Changes: []state.Change{{Key: claim.laneKey, Requirement: state.RequirementPresent, ExpectedVersion: laneValue.Version, Data: body}}}); err != nil {
				t.Fatal(err)
			}
			err = s.finishThumbnail(t.Context(), claim, ItemResult{State: StateReady}, nil)
			if corrupt && !errors.Is(err, domain.ErrInvalid) || !corrupt && err != nil {
				t.Fatalf("stale completion: %v", err)
			}
			job, _, err := s.loadThumbnail(t.Context(), claim.job.ID)
			if err != nil || job.State != StateGenerating || job.LeaseID != claim.job.LeaseID {
				t.Fatalf("stale completion consumed the job: %+v %v", job, err)
			}
			current, err := s.state.Get(t.Context(), claim.laneKey)
			if err != nil || string(current.Data) != string(body) {
				t.Fatalf("stale completion released a replaced lane: %v", err)
			}
		})
	}
}

func TestThumbnailQueueCleanupFailuresPreserveRetainedFailure(t *testing.T) {
	for _, boundary := range []string{"job-read", "index-read", "write", "conflict"} {
		t.Run(boundary, func(t *testing.T) {
			s, binding, item, clock := queueTestService(t)
			base := s.state
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			claim, found, err := s.claimThumbnail(t.Context())
			if err != nil || !found {
				t.Fatal(err)
			}
			if err := s.finishThumbnail(t.Context(), claim, ItemResult{State: StateFailed}, nil); err != nil {
				t.Fatal(err)
			}
			clock.Advance(s.options.OperationRetention)
			s.state = queueBoundaryFailure(base, boundary)
			if removed, err := s.cleanupThumbnail(t.Context()); removed || boundary != "conflict" && !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("cleanup failure=%v %v", removed, err)
			}
			s.state = base
			if removed, err := s.cleanupThumbnail(t.Context()); err != nil || !removed {
				t.Fatalf("cleanup retry=%v %v", removed, err)
			}
		})
	}
}

func TestThumbnailQueueChangedLocatorAndExistingGenerationRemainPending(t *testing.T) {
	for _, generationInProgress := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-locator", true: "other-generator"}[generationInProgress], func(t *testing.T) {
			s, binding, item, clock := queueTestService(t)
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			claim, found, err := s.claimThumbnail(t.Context())
			if err != nil || !found {
				t.Fatal(err)
			}
			result, workErr := ItemResult{}, error(domain.ErrNotFound)
			if generationInProgress {
				result.State = StateGenerating
				workErr = nil
			} else {
				item.Path = domain.MustParseUserPath("/moved.png")
				item.Version = "moved-version"
				clock.Advance(time.Second)
				if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.finishThumbnail(t.Context(), claim, result, workErr); err != nil {
				t.Fatal(err)
			}
			job, _, err := s.loadThumbnail(t.Context(), claim.job.ID)
			if err != nil || job.State != StateQueued || job.Attempts != 0 {
				t.Fatalf("recoverable work became terminal: %+v %v", job, err)
			}
		})
	}
}

func TestThumbnailQueueIgnoresReplacedPrioritySnapshot(t *testing.T) {
	s, binding, item, clock := queueTestService(t)
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	base, refresh := s.state, true
	s.state = queueFaultStore{AtomicStore: base, list: func(ctx context.Context, prefix state.Prefix, request state.PageRequest) (state.Page, error) {
		page, err := base.List(ctx, prefix, request)
		if refresh && err == nil {
			refresh = false
			clock.Advance(time.Second)
			if _, err := s.enqueueThumbnail(ctx, binding, item); err != nil {
				return page, err
			}
		}
		return page, err
	}}
	if _, found, err := s.claimThumbnail(t.Context()); err != nil || found {
		t.Fatalf("stale priority snapshot not skipped: %v %v", found, err)
	}
	s.state = base
	if _, found, err := s.claimThumbnail(t.Context()); err != nil || !found {
		t.Fatalf("refreshed job lost: %v %v", found, err)
	}
}

func TestThumbnailQueueCorruptIndexCannotConsumeValidWork(t *testing.T) {
	for _, kind := range []string{"malformed", "unknown-job", "misbound"} {
		t.Run(kind, func(t *testing.T) {
			s, binding, item, clock := queueTestService(t)
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			id := thumbnailID(binding)
			if kind == "unknown-job" {
				other := binding
				other.RecipeID = "another-recipe"
				id = thumbnailID(other)
			}
			body, _ := state.EncodeJSON(thumbnailIndex{SchemaVersion: 1, JobID: id})
			if kind == "malformed" {
				body = []byte(`{}`)
			}
			key := thumbnailKey("recent", thumbnailOrder(clock.Now().Add(time.Hour), true), id)
			version, err := s.state.Create(t.Context(), key, body)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := s.claimThumbnail(t.Context()); err == nil || found {
				t.Fatalf("corrupt index accepted: %v %v", found, err)
			}
			job, _, err := s.loadThumbnail(t.Context(), thumbnailID(binding))
			if err != nil || job.State != StateQueued {
				t.Fatalf("corrupt index consumed real work: %+v %v", job, err)
			}
			if err := s.state.Delete(t.Context(), key, version); err != nil {
				t.Fatal(err)
			}
			if _, found, err := s.claimThumbnail(t.Context()); err != nil || !found {
				t.Fatalf("valid work lost: %v %v", found, err)
			}
		})
	}
}

func TestThumbnailQueueCorruptLaneAndIdentityFailureLeaveJobPending(t *testing.T) {
	for _, kind := range []string{"lane", "entropy"} {
		t.Run(kind, func(t *testing.T) {
			s, binding, item, _ := queueTestService(t)
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			if kind == "lane" {
				if _, err := s.state.Create(t.Context(), thumbnailKey("lane", "0"), []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
			} else {
				s.ids = domain.NewIDGenerator(strings.NewReader(""))
			}
			if _, found, err := s.claimThumbnail(t.Context()); err == nil || found {
				t.Fatalf("invalid claim accepted: %v %v", found, err)
			}
			job, _, err := s.loadThumbnail(t.Context(), thumbnailID(binding))
			if err != nil || job.State != StateQueued {
				t.Fatalf("failed claim lost job: %+v %v", job, err)
			}
		})
	}
}

func TestThumbnailQueueCorruptRetentionCannotDeleteOtherWork(t *testing.T) {
	for _, kind := range []string{"malformed", "misbound"} {
		t.Run(kind, func(t *testing.T) {
			s, binding, item, clock := queueTestService(t)
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			body, _ := state.EncodeJSON(thumbnailIndex{SchemaVersion: 1, JobID: thumbnailID(binding)})
			if kind == "malformed" {
				body = []byte(`{}`)
			}
			if _, err := s.state.Create(t.Context(), thumbnailKey("terminal", thumbnailOrder(clock.Now(), false), thumbnailID(binding)), body); err != nil {
				t.Fatal(err)
			}
			if removed, err := s.cleanupThumbnail(t.Context()); err == nil || removed {
				t.Fatalf("bad retention deleted work: %v %v", removed, err)
			}
			if _, _, err := s.loadThumbnail(t.Context(), thumbnailID(binding)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestThumbnailQueueSourceFailureLeavesRetryAndDisabledWorkerDoesNoWork(t *testing.T) {
	s, binding, item, _ := queueTestService(t)
	s.store = &scriptedStore{}
	s.options.Automatic = true
	s.source = &scriptedStorage{statErr: domain.ErrUnavailable}
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.WorkOne(t.Context()); err != nil || !worked {
		t.Fatalf("source failure processing: %v %v", worked, err)
	}
	job, _, err := s.loadThumbnail(t.Context(), thumbnailID(binding))
	if err != nil || job.State != StateQueued || job.Attempts != 1 {
		t.Fatalf("source failure lost retry: %+v %v", job, err)
	}
	s.options.Automatic = false
	if worked, err := s.WorkOne(t.Context()); err != nil || worked {
		t.Fatalf("disabled worker ran: %v %v", worked, err)
	}
	s.Run(t.Context())
}

type queueBatchReader struct {
	state.AtomicStore
	values []state.Value
	err    error
}

func (s queueBatchReader) GetMany(context.Context, []state.Key) ([]state.Value, error) {
	return s.values, s.err
}

func TestThumbnailQueueBatchReadFailuresNeverBecomeAbsence(t *testing.T) {
	for _, kind := range []string{"unavailable", "shape", "corrupt", "cold"} {
		t.Run(kind, func(t *testing.T) {
			s, binding, item, _ := queueTestService(t)
			reader := queueBatchReader{AtomicStore: s.state}
			switch kind {
			case "unavailable":
				reader.err = domain.ErrUnavailable
			case "corrupt":
				reader.values = []state.Value{{Data: []byte(`{}`), Version: "v1"}}
			case "cold":
				reader.err = domain.ErrNotFound
			}
			s.state = reader
			got, err := s.enqueueThumbnail(t.Context(), binding, item)
			if kind == "cold" {
				if err != nil || got != StateQueued {
					t.Fatalf("cold batch did not use exact lookup: %s %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s batch admitted unknown state", kind)
			}
			if _, _, readErr := s.loadThumbnail(t.Context(), thumbnailID(binding)); !errors.Is(readErr, domain.ErrNotFound) {
				t.Fatalf("failed batch published a job: %v", readErr)
			}
		})
	}
}
