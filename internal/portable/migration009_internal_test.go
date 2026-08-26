package portable

import (
	"bytes"
	"testing"

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
