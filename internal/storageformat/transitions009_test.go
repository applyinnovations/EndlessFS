package storageformat

import (
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestSchema009TransitionRecordsRejectInvalidRelationships(t *testing.T) {
	fingerprint := Digest([]byte("transition"))
	validChange := TransitionChange009{Key: "value", Requirement: 1, Value: []byte("value")}
	validParticipants := []TransitionParticipant009{
		{Kind: DomainAdmin, DomainID: "administration", Changes: []TransitionChange009{validChange}},
		{Kind: DomainNamespace, DomainID: "owner", Changes: []TransitionChange009{validChange}},
	}
	base := TransitionPlan009{SchemaVersion: 1, TransitionID: "transition", Fingerprint: fingerprint, RetainUntil: time.Date(2048, 1, 2, 3, 4, 5, 0, time.UTC), Participants: validParticipants}
	for name, mutate := range map[string]func(*TransitionPlan009){
		"plan":        func(plan *TransitionPlan009) { plan.SchemaVersion = 0 },
		"participant": func(plan *TransitionPlan009) { plan.Participants[0].DomainID = "" },
		"change":      func(plan *TransitionPlan009) { plan.Participants[0].Changes[0].Requirement = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Participants = append([]TransitionParticipant009(nil), base.Participants...)
			for index := range candidate.Participants {
				candidate.Participants[index].Changes = append([]TransitionChange009(nil), base.Participants[index].Changes...)
			}
			mutate(&candidate)
			if err := ValidateTransitionPlan009(candidate); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid plan error = %v", err)
			}
		})
	}
	if err := ValidateTransitionDecision009(TransitionDecision009{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid decision error = %v", err)
	}
	if err := ValidateTransitionLock009(TransitionLock009{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid lock error = %v", err)
	}
}
