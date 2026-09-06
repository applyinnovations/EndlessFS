package storageformat

import (
	"bytes"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	StateRecordProfile                 = "profile"
	StateRecordAccount                 = "account"
	StateRecordCredential              = "credential"
	StateRecordCredentialIndex         = "credential-index" // #nosec G101 -- canonical record-type label, never credential material.
	StateRecordCeremony                = "ceremony"
	StateRecordRegistrationOperation   = "registration-operation"
	StateRecordAuthenticationOperation = "authentication-operation"
	StateRecordBootstrap               = "bootstrap"
	StateRecordFirstAccount            = "first-account"
	StateRecordIdempotency             = "idempotency"
	StateRecordSession                 = "session"
	StateRecordInvite                  = "invite"
	StateRecordRecovery                = "recovery"
	StateRecordShare                   = "share"
	StateRecordTrash                   = "trash"
	StateRecordUpload                  = "upload"
	StateRecordBatchOperation          = "batch-operation"
	StateRecordMutationOutcome         = "mutation-outcome"
	StateRecordThemePreference         = "theme-preference"
	StateRecordAdminRoles              = "admin-roles"
	StateRecordPreviewOperation        = "preview-operation"
	StateRecordPreviewIdempotency      = "preview-idempotency"
	StateRecordPreviewIndex            = "preview-index"
	StateRecordTransitionDecision      = "transition-decision"
	StateRecordCleanupTask             = "cleanup-task"
	StateRecordGarbageCollection       = "garbage-collection"
)

// StateRecord009 is the typed canonical envelope for application state stored
// inside a schema-009 consistency-domain value. The payload remains the
// application record's strict JSON so the state interface does not expose
// storage-format details to callers.
type StateRecord009 struct {
	SchemaVersion int    `json:"schemaVersion"`
	RecordType    string `json:"recordType"`
	Payload       []byte `json:"payload"`
}

func validateStateRecord009(record StateRecord009, expectedType string) error {
	if record.SchemaVersion != 1 || !validDomainText(record.RecordType) || expectedType != "" && record.RecordType != expectedType {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-009 state record binding")
	}
	if len(record.Payload) == 0 || len(record.Payload) > state.MaxRecordBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-009 state record payload")
	}
	return nil
}

func EncodeStateRecord009(recordType string, payload []byte) ([]byte, error) {
	record := StateRecord009{SchemaVersion: 1, RecordType: recordType, Payload: append([]byte(nil), payload...)}
	if err := validateStateRecord009(record, recordType); err != nil {
		return nil, err
	}
	return EncodeCanonical(record)
}

func DecodeStateRecord009(data []byte, expectedType string) ([]byte, error) {
	var record StateRecord009
	if err := state.DecodeJSONWithLimit(data, &record, MaxCanonicalBytes); err != nil {
		return nil, err
	}
	if err := validateStateRecord009(record, expectedType); err != nil {
		return nil, err
	}
	canonical, err := EncodeCanonical(record)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, data) {
		return nil, domain.NewError(domain.ErrorInvalid, "non-canonical schema-009 state record")
	}
	return append([]byte(nil), record.Payload...), nil
}
