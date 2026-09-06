package portable

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema009MigrationRekeysLegacyCapabilityStateByOwner(t *testing.T) {
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	tests := []struct {
		name       string
		key        state.Key
		payload    []byte
		wantKey    state.Key
		wantType   string
		wantDomain consistencyDomainRef
	}{
		{
			name: "credential", key: state.MustKey(state.NamespaceCredentials, "credential-hash"),
			payload: []byte(`{"schemaVersion":1,"userID":"` + owner + `","credentialID":"credential"}`),
			wantKey: state.MustKey(state.NamespaceCredentials, owner, "credential-hash"), wantType: storageformat.StateRecordCredential,
			wantDomain: consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:" + owner},
		},
		{
			name: "session", key: state.MustKey(state.NamespaceSessions, "token-hash"),
			payload: []byte(`{"schemaVersion":1,"userID":"` + owner + `"}`),
			wantKey: state.MustKey(state.NamespaceSessions, owner, "token-hash"), wantType: storageformat.StateRecordSession,
			wantDomain: consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:" + owner},
		},
		{
			name: "share", key: state.MustKey(state.NamespaceShares, "token-hash"),
			payload: []byte(`{"schemaVersion":1,"ownerUserID":"` + owner + `","shareID":"share-a"}`),
			wantKey: state.MustKey(state.NamespaceShares, owner, "share-a"), wantType: storageformat.StateRecordShare,
			wantDomain: consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: owner},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, reference, recordType, body, err := migrateStateEntry009(test.key, test.payload)
			if err != nil {
				t.Fatal(err)
			}
			if key.String() != test.wantKey.String() || reference != test.wantDomain || recordType != test.wantType {
				t.Fatalf("migration = %s, %+v, %q; want %s, %+v, %q", key.String(), reference, recordType, test.wantKey.String(), test.wantDomain, test.wantType)
			}
			decoded, err := storageformat.DecodeStateRecord009(body, test.wantType)
			if err != nil || !bytes.Equal(decoded, test.payload) {
				t.Fatalf("decoded payload = %s, %v; want %s", decoded, err, test.payload)
			}
		})
	}
}

func TestSchema009MigrationRejectsMalformedStageAndStateBindings(t *testing.T) {
	validStage := schema009MigrationStage{
		SchemaVersion:  schema009MigrationStageSchema,
		SourceIdentity: "source", DomainKind: storageformat.DomainIdentity,
		DomainID: "owner:fixture", Tree: "base", Key: "users/fixture",
		Value: []byte("value"), LogicalVersion: "version",
	}
	if _, body, err := validateSchema009MigrationStage(validStage); err != nil || len(body) == 0 {
		t.Fatalf("valid stage = %q, %v", body, err)
	}
	invalidStages := []schema009MigrationStage{
		{},
		func() schema009MigrationStage { value := validStage; value.Tree = "unknown"; return value }(),
		func() schema009MigrationStage { value := validStage; value.DomainKind = "unknown"; return value }(),
	}
	for index, stage := range invalidStages {
		if _, _, err := validateSchema009MigrationStage(stage); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid stage %d error = %v", index, err)
		}
	}

	for _, test := range []struct {
		name    string
		payload []byte
		field   string
	}{
		{name: "malformed", payload: []byte("{"), field: "userID"},
		{name: "missing", payload: []byte(`{}`), field: "userID"},
		{name: "wrong-type", payload: []byte(`{"userID":1}`), field: "userID"},
		{name: "empty", payload: []byte(`{"userID":""}`), field: "userID"},
	} {
		t.Run("required/"+test.name, func(t *testing.T) {
			if _, err := schema009StringField(test.payload, test.field); err == nil {
				t.Fatal("malformed required binding was accepted")
			}
		})
		t.Run("optional/"+test.name, func(t *testing.T) {
			if _, _, err := schema009OptionalStringField(test.payload, test.field); test.name == "missing" {
				if err != nil {
					t.Fatalf("missing optional binding = %v", err)
				}
			} else if err == nil {
				t.Fatal("malformed optional binding was accepted")
			}
		})
	}
	if value, found, err := schema009OptionalStringField([]byte(`{"userID":null}`), "userID"); err != nil || found || value != "" {
		t.Fatalf("null optional binding = %q, %t, %v", value, found, err)
	}
	if value, found, err := schema009OptionalStringField([]byte(`{"userID":"owner"}`), "userID"); err != nil || !found || value != "owner" {
		t.Fatalf("valid optional binding = %q, %t, %v", value, found, err)
	}
	oversizedStage := validStage
	oversizedStage.Value = bytes.Repeat([]byte("x"), storageformat.MaxCanonicalBytes+1)
	if _, _, err := validateSchema009MigrationStage(oversizedStage); err == nil {
		t.Fatal("oversized migration stage was accepted")
	}
}

func TestSchema009MigrationClassifiesEveryDurableStateFamily(t *testing.T) {
	tests := []struct {
		namespace state.Namespace
		parts     []string
		want      string
	}{
		{state.NamespaceUsers, []string{"owner"}, storageformat.StateRecordProfile},
		{state.NamespaceAccounts, []string{"owner"}, storageformat.StateRecordAccount},
		{state.NamespaceCredentials, []string{"owner", "index"}, storageformat.StateRecordCredentialIndex},
		{state.NamespaceCredentials, []string{"owner", "credential"}, storageformat.StateRecordCredential},
		{state.NamespaceCeremonies, []string{"capability", "ceremony"}, storageformat.StateRecordCeremony},
		{state.NamespaceSessions, []string{"owner", "session"}, storageformat.StateRecordSession},
		{state.NamespaceInvites, []string{"invite"}, storageformat.StateRecordInvite},
		{state.NamespaceRecoveries, []string{"owner", "recovery"}, storageformat.StateRecordRecovery},
		{state.NamespaceShares, []string{"owner", "share"}, storageformat.StateRecordShare},
		{state.NamespaceTrash, []string{"owner"}, storageformat.StateRecordTrash},
		{state.NamespaceUploads, []string{"upload"}, storageformat.StateRecordUpload},
		{state.NamespaceBootstrap, []string{"first-account"}, storageformat.StateRecordFirstAccount},
		{state.NamespaceBootstrap, []string{"state"}, storageformat.StateRecordBootstrap},
		{state.NamespaceRoles, []string{"admins"}, storageformat.StateRecordAdminRoles},
		{state.NamespacePreferences, []string{"owner", "theme"}, storageformat.StateRecordThemePreference},
		{state.NamespaceIdempotency, []string{"preview", "request"}, storageformat.StateRecordPreviewIdempotency},
		{state.NamespaceIdempotency, []string{"identity", "owner", "request"}, storageformat.StateRecordIdempotency},
		{state.NamespaceOperations, []string{"preview", "operation"}, storageformat.StateRecordPreviewOperation},
		{state.NamespaceOperations, []string{"preview-index", "operation"}, storageformat.StateRecordPreviewIndex},
		{state.NamespaceOperations, []string{"batch", "operation"}, storageformat.StateRecordBatchOperation},
		{state.NamespaceOperations, []string{"identity", "owner", "authentication", "operation"}, storageformat.StateRecordAuthenticationOperation},
		{state.NamespaceOperations, []string{"identity", "owner", "registration", "operation"}, storageformat.StateRecordRegistrationOperation},
		{state.NamespaceOperations, []string{"admin", "operation"}, storageformat.StateRecordMutationOutcome},
	}
	for _, test := range tests {
		if got, err := stateRecordType009(test.namespace, test.parts); err != nil || got != test.want {
			t.Errorf("record type %s/%v = %q, %v; want %q", test.namespace, test.parts, got, err, test.want)
		}
	}
	if _, err := stateRecordType009(state.Namespace("unknown"), nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown state family error = %v", err)
	}
}

func TestSchema009MigrationRejectsStateWithoutAnExactConsistencyDomainRoute(t *testing.T) {
	if _, _, _, _, err := migrateStateEntry009(state.MustKey(state.NamespaceOperations, "unknown"), []byte(`{}`)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unroutable state migration error = %v", err)
	}
}

func TestSchema009MigrationRelocationDeniesEveryMalformedOwnerBinding(t *testing.T) {
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	valid := []struct {
		key     state.Key
		payload []byte
	}{
		{state.MustKey(state.NamespaceCredentials, "user-index", owner), []byte(`{}`)},
		{state.MustKey(state.NamespaceCeremonies, "ceremony"), []byte(`{"userID":"` + owner + `"}`)},
		{state.MustKey(state.NamespaceCeremonies, "capability"), []byte(`{}`)},
		{state.MustKey(state.NamespaceRecoveries, "recovery"), []byte(`{"targetUserID":"` + owner + `"}`)},
		{state.MustKey(state.NamespaceIdempotency, owner, "request"), []byte(`{}`)},
		{state.MustKey(state.NamespaceOperations, "registration", "operation"), []byte(`{"userID":"` + owner + `"}`)},
	}
	for index, test := range valid {
		if _, _, _, _, err := migrateStateEntry009(test.key, test.payload); err != nil {
			t.Fatalf("valid relocation %d: %v", index, err)
		}
	}

	invalid := []state.Key{
		state.MustKey(state.NamespaceCredentials, "credential"),
		state.MustKey(state.NamespaceSessions, "session"),
		state.MustKey(state.NamespaceRecoveries, "recovery"),
		state.MustKey(state.NamespaceShares, "share"),
		state.MustKey(state.NamespaceOperations, "registration", "operation"),
	}
	for _, key := range invalid {
		if _, _, _, _, err := migrateStateEntry009(key, []byte(`{}`)); err == nil {
			t.Errorf("missing owner binding for %s was accepted", key.String())
		}
	}
	if _, _, _, _, err := migrateStateEntry009(state.Key{}, nil); err == nil {
		t.Fatal("invalid logical state key was accepted")
	}
	if _, _, _, _, err := migrateStateEntry009(state.MustKey(state.NamespaceShares, "share"), []byte(`{"ownerUserID":"`+owner+`"}`)); err == nil {
		t.Fatal("share without share ID was accepted")
	}
	if _, _, _, _, err := migrateStateEntry009(state.MustKey(state.NamespaceCeremonies, "ceremony"), []byte(`{"userID":1}`)); err == nil {
		t.Fatal("ceremony with malformed optional owner was accepted")
	}
	oversized := []byte(strings.Repeat("x", state.MaxRecordBytes+1))
	if _, _, _, _, err := migrateStateEntry009(state.MustKey(state.NamespacePreferences, owner, "theme"), oversized); err == nil {
		t.Fatal("oversized state record was accepted")
	}
}
