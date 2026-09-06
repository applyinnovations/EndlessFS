package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type mutationFingerprintChange struct {
	Key             string      `json:"key"`
	Requirement     Requirement `json:"requirement"`
	ExpectedVersion Version     `json:"expectedVersion,omitempty"`
	Delete          bool        `json:"delete,omitempty"`
	Data            []byte      `json:"data,omitempty"`
}

// NormalizeMutation validates and defensively copies a mutation, sorts its
// changes by canonical key, and returns the stable intent fingerprint used by
// deterministic AtomicStore implementations.
func NormalizeMutation(mutation Mutation) (Mutation, string, error) {
	if mutation.ID == "" || !utf8.ValidString(mutation.ID) || len(mutation.Changes) == 0 {
		return Mutation{}, "", domain.NewError(domain.ErrorInvalid, "invalid atomic state mutation")
	}
	if len(mutation.Result) > MaxRecordBytes {
		return Mutation{}, "", domain.NewError(domain.ErrorInvalid, "atomic state mutation result exceeds size limit")
	}
	changes := append([]Change(nil), mutation.Changes...)
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key.String() < changes[right].Key.String() })
	fingerprintChanges := make([]mutationFingerprintChange, len(changes))
	previous := ""
	for index := range changes {
		change := &changes[index]
		if err := validateKey(change.Key); err != nil {
			return Mutation{}, "", err
		}
		if change.Key.String() == previous {
			return Mutation{}, "", domain.NewError(domain.ErrorInvalid, "atomic state mutation contains a duplicate key")
		}
		previous = change.Key.String()
		if change.Requirement < RequirementAny || change.Requirement > RequirementPresent {
			return Mutation{}, "", domain.NewError(domain.ErrorInvalid, "invalid atomic state precondition")
		}
		if change.Requirement != RequirementPresent && change.ExpectedVersion != "" {
			return Mutation{}, "", domain.NewError(domain.ErrorInvalid, "atomic state version requires a present precondition")
		}
		if change.Delete && (change.Requirement != RequirementPresent || len(change.Data) != 0) {
			return Mutation{}, "", domain.NewError(domain.ErrorInvalid, "invalid atomic state deletion")
		}
		if !change.Delete {
			if err := validateRecordData(change.Data); err != nil {
				return Mutation{}, "", err
			}
		}
		change.Data = append([]byte(nil), change.Data...)
		fingerprintChanges[index] = mutationFingerprintChange{
			Key:             change.Key.String(),
			Requirement:     change.Requirement,
			ExpectedVersion: change.ExpectedVersion,
			Delete:          change.Delete,
			Data:            append([]byte(nil), change.Data...),
		}
	}
	intent := struct {
		Changes []mutationFingerprintChange `json:"changes"`
		Result  []byte                      `json:"result,omitempty"`
	}{Changes: fingerprintChanges, Result: append([]byte(nil), mutation.Result...)}
	body, err := json.Marshal(intent)
	if err != nil {
		return Mutation{}, "", domain.WrapError(domain.ErrorInvalid, "encode atomic state mutation", err)
	}
	digest := sha256.Sum256(append([]byte("endlessfs-atomic-state-mutation-v1\x00"), body...))
	mutation.Changes = changes
	mutation.Result = append([]byte(nil), mutation.Result...)
	mutation.RetainUntil = mutation.RetainUntil.UTC()
	return mutation, hex.EncodeToString(digest[:]), nil
}

func cloneMutationOutcome(outcome MutationOutcome) MutationOutcome {
	cloned := outcome
	cloned.Result = append([]byte(nil), outcome.Result...)
	cloned.Changes = append([]ChangeResult(nil), outcome.Changes...)
	return cloned
}
