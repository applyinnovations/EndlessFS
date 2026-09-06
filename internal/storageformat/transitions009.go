package storageformat

import (
	"sort"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type TransitionChange009 struct {
	Key             string `json:"key"`
	Requirement     uint8  `json:"requirement"`
	ExpectedVersion string `json:"expectedVersion,omitempty"`
	Delete          bool   `json:"delete,omitempty"`
	Value           []byte `json:"value,omitempty"`
}

type TransitionParticipant009 struct {
	Kind     ConsistencyDomainKind `json:"kind"`
	DomainID string                `json:"domainID"`
	Changes  []TransitionChange009 `json:"changes"`
}

type TransitionPlan009 struct {
	SchemaVersion int                        `json:"schemaVersion"`
	TransitionID  string                     `json:"transitionID"`
	Fingerprint   string                     `json:"fingerprint"`
	RetainUntil   time.Time                  `json:"retainUntil"`
	Participants  []TransitionParticipant009 `json:"participants"`
	Result        []byte                     `json:"result,omitempty"`
}

type TransitionDecision009 struct {
	SchemaVersion int       `json:"schemaVersion"`
	TransitionID  string    `json:"transitionID"`
	Fingerprint   string    `json:"fingerprint"`
	Committed     bool      `json:"committed"`
	ErrorKind     string    `json:"errorKind,omitempty"`
	DecidedAt     time.Time `json:"decidedAt"`
}

type TransitionLock009 struct {
	SchemaVersion int                   `json:"schemaVersion"`
	TransitionID  string                `json:"transitionID"`
	Fingerprint   string                `json:"fingerprint"`
	Kind          ConsistencyDomainKind `json:"kind"`
	DomainID      string                `json:"domainID"`
}

func ValidateTransitionPlan009(plan TransitionPlan009) error {
	if plan.SchemaVersion != 1 || !validDomainText(plan.TransitionID) || !validDomainDigest(plan.Fingerprint) || plan.RetainUntil.IsZero() || len(plan.Participants) < 2 || len(plan.Participants) > 16 {
		return domain.NewError(domain.ErrorInvalid, "invalid transition plan")
	}
	previousParticipant := ""
	for _, participant := range plan.Participants {
		ordering := string(participant.Kind) + "\x00" + participant.DomainID
		if !validDomainKind(participant.Kind) || !validDomainText(participant.DomainID) || ordering <= previousParticipant || len(participant.Changes) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid transition participant")
		}
		previousParticipant = ordering
		previousKey := ""
		for _, change := range participant.Changes {
			if !validDomainText(change.Key) || change.Key <= previousKey || change.Requirement < 1 || change.Requirement > 3 || change.Requirement != 3 && change.ExpectedVersion != "" || change.Delete && (change.Requirement != 3 || len(change.Value) != 0) {
				return domain.NewError(domain.ErrorInvalid, "invalid transition change")
			}
			previousKey = change.Key
		}
	}
	_, err := EncodeCanonical(plan)
	return err
}

func ValidateTransitionDecision009(decision TransitionDecision009) error {
	if decision.SchemaVersion != 1 || !validDomainText(decision.TransitionID) || !validDomainDigest(decision.Fingerprint) || decision.DecidedAt.IsZero() || decision.Committed && decision.ErrorKind != "" || !decision.Committed && decision.ErrorKind != string(domain.ErrorConflict) && decision.ErrorKind != string(domain.ErrorNotFound) && decision.ErrorKind != string(domain.ErrorPreconditionFailed) && decision.ErrorKind != string(domain.ErrorInvalid) {
		return domain.NewError(domain.ErrorInvalid, "invalid transition decision")
	}
	_, err := EncodeCanonical(decision)
	return err
}

func ValidateTransitionLock009(lock TransitionLock009) error {
	if lock.SchemaVersion != 1 || !validDomainText(lock.TransitionID) || !validDomainDigest(lock.Fingerprint) || !validDomainKind(lock.Kind) || !validDomainText(lock.DomainID) {
		return domain.NewError(domain.ErrorInvalid, "invalid transition lock")
	}
	_, err := EncodeCanonical(lock)
	return err
}

func SortTransitionParticipants009(participants []TransitionParticipant009) {
	sort.Slice(participants, func(left, right int) bool {
		return string(participants[left].Kind)+"\x00"+participants[left].DomainID < string(participants[right].Kind)+"\x00"+participants[right].DomainID
	})
}
