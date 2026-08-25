package portable

import (
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema009StateRoutingCoLocatesTransactionalInvariants(t *testing.T) {
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	cases := []struct {
		name string
		key  state.Key
		kind storageformat.ConsistencyDomainKind
		id   string
	}{
		{"profile", state.MustKey(state.NamespaceUsers, owner), storageformat.DomainIdentity, "owner:" + owner},
		{"account", state.MustKey(state.NamespaceAccounts, owner), storageformat.DomainIdentity, "owner:" + owner},
		{"credential", state.MustKey(state.NamespaceCredentials, owner, "credential-hash"), storageformat.DomainIdentity, "owner:" + owner},
		{"credential-index", state.MustKey(state.NamespaceCredentials, owner, "index"), storageformat.DomainIdentity, "owner:" + owner},
		{"session", state.MustKey(state.NamespaceSessions, owner, "token-hash"), storageformat.DomainIdentity, "owner:" + owner},
		{"owner-ceremony", state.MustKey(state.NamespaceCeremonies, "owner", owner, "ceremony-hash"), storageformat.DomainIdentity, "owner:" + owner},
		{"recovery", state.MustKey(state.NamespaceRecoveries, owner, "token-hash"), storageformat.DomainIdentity, "owner:" + owner},
		{"identity-operation", state.MustKey(state.NamespaceOperations, "identity", owner, "operation"), storageformat.DomainIdentity, "owner:" + owner},
		{"identity-idempotency", state.MustKey(state.NamespaceIdempotency, "identity", owner, "request"), storageformat.DomainIdentity, "owner:" + owner},
		{"upload", state.MustKey(state.NamespaceUploads, owner, "upload"), storageformat.DomainNamespace, owner},
		{"trash", state.MustKey(state.NamespaceTrash, owner, "trash"), storageformat.DomainNamespace, owner},
		{"share", state.MustKey(state.NamespaceShares, owner, "token-hash"), storageformat.DomainNamespace, owner},
		{"drive-idempotency", state.MustKey(state.NamespaceIdempotency, "drive", owner, "request"), storageformat.DomainNamespace, owner},
		{"batch", state.MustKey(state.NamespaceOperations, "batch", owner, "operation"), storageformat.DomainNamespace, owner},
		{"preview-operation", state.MustKey(state.NamespaceOperations, "preview", owner, "operation"), storageformat.DomainOwnerJobs, "owner:" + owner},
		{"preview-index", state.MustKey(state.NamespaceOperations, "preview-index", owner, "shard"), storageformat.DomainOwnerJobs, "owner:" + owner},
		{"preview-idempotency", state.MustKey(state.NamespaceIdempotency, "preview", owner, "request"), storageformat.DomainOwnerJobs, "owner:" + owner},
		{"bootstrap", state.MustKey(state.NamespaceBootstrap, "state"), storageformat.DomainAdmin, "administration"},
		{"roles", state.MustKey(state.NamespaceRoles, "admins"), storageformat.DomainAdmin, "administration"},
		{"invite", state.MustKey(state.NamespaceInvites, "token-hash"), storageformat.DomainAdmin, "administration"},
		{"admin-operation", state.MustKey(state.NamespaceOperations, "admin", "operation"), storageformat.DomainAdmin, "administration"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			reference, err := stateDomainReferenceForKey009(test.key)
			if err != nil || reference.Kind != test.kind || reference.ID != test.id {
				t.Fatalf("route %q = %+v, %v; want %s/%s", test.key.String(), reference, err, test.kind, test.id)
			}
		})
	}
}

func TestSchema009StateRoutingRejectsLegacyUnscopedSensitiveKeys(t *testing.T) {
	for _, key := range []state.Key{
		state.MustKey(state.NamespaceCredentials, "credential-hash"),
		state.MustKey(state.NamespaceSessions, "token-hash"),
		state.MustKey(state.NamespaceRecoveries, "token-hash"),
		state.MustKey(state.NamespaceShares, "token-hash"),
		state.MustKey(state.NamespaceOperations, "registration", "operation"),
	} {
		if _, err := stateDomainReferenceForKey009(key); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("legacy unscoped key %q error = %v", key.String(), err)
		}
	}
}

func TestSchema009OwnerUnknownCeremonyRemainsCapabilitySharded(t *testing.T) {
	first, err := stateDomainReferenceForKey009(state.MustKey(state.NamespaceCeremonies, "capability", "first-hash"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := stateDomainReferenceForKey009(state.MustKey(state.NamespaceCeremonies, "capability", "second-hash"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != storageformat.DomainCapability || second.Kind != storageformat.DomainCapability || first == second {
		t.Fatalf("owner-unknown ceremony domains = %+v and %+v", first, second)
	}
}
