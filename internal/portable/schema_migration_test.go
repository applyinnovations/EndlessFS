package portable_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
	"github.com/descope/virtualwebauthn"
)

const migrationCandidateReleaseEnvironment = "ENDLESSFS_MIGRATION_CANDIDATE_RELEASE"

type storageSchemaFixture struct {
	SchemaVersion  int                      `json:"schemaVersion"`
	SourceRelease  string                   `json:"sourceRelease"`
	SourceCommit   string                   `json:"sourceCommit"`
	CreatedAt      time.Time                `json:"createdAt"`
	UserID         string                   `json:"userID"`
	StateObjects   map[string][]byte        `json:"stateObjects"`
	FileObjects    map[string][]byte        `json:"fileObjects"`
	SemanticOracle *migrationSemanticOracle `json:"semanticOracle,omitempty"`
}

type migrationSemanticOracle struct {
	UserID        string                        `json:"userID"`
	DisplayName   string                        `json:"displayName"`
	CredentialID  string                        `json:"credentialID"`
	Authenticator virtualwebauthn.Authenticator `json:"authenticator"`
	RequiredKeys  []string                      `json:"requiredKeys"`
}

type storageSchemaFixtureEntry struct {
	schemaID  string
	profile   string
	file      string
	digest    string
	producer  string
	commit    string
	wantSize  int64
	wantFiles int64
}

var storageSchemaFixtures = []storageSchemaFixtureEntry{
	{
		schemaID: "endlessfs-portable-v1/schema-001",
		profile:  "portable-minimal",
		file:     "pre-aggregate-v0.1.4.json", digest: "24111f7739207b53fad5c4e1cf0ca106982b40fce33850f045d7430150260258",
		producer: "v0.1.4", commit: "edb67f8e345694001b9614604c5baded9bde5d86", wantSize: 26, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-002",
		profile:  "portable-minimal",
		file:     "schema-002-recursive-bytes.json", digest: "c7fc6a6924e62f99e9fdd99a35343385c17088d36bcac5f47b6abfe8776ee854",
		producer: "schema-002", commit: "b70f6361497d45f20049279bb5381a4fbb1005f1", wantSize: 10, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-003",
		profile:  "portable-minimal",
		file:     "recursive-aggregates-v0.1.7.json", digest: "0e2ce0a0853cba6e29730346b69e3c829240f617b1f277949f394b9a54786a51",
		producer: "v0.1.7", commit: "1548dafa30ea3fbf0340b3b32381e885a110ef5e", wantSize: 26, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-001",
		profile:  "application-preview-disabled",
		file:     "pre-aggregate-v0.1.4-application-disabled.json", digest: "b6932210f53e927bf0543153290674579e50f0004bdad1e1e474256fbea8e15a",
		producer: "v0.1.4", commit: "edb67f8e345694001b9614604c5baded9bde5d86", wantSize: 22, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-001",
		profile:  "application-preview-gcs",
		file:     "pre-aggregate-v0.1.4-application-gcs.json", digest: "8e508619ffb77850403f2e83de9d1ce98dabfe330334ffee9c2e87f6c250cab8",
		producer: "v0.1.4", commit: "edb67f8e345694001b9614604c5baded9bde5d86", wantSize: 22, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-001",
		profile:  "application-preview-gcs-v0.1.7-interrupted",
		file:     "schema-001-v0.1.7-interrupted-application-gcs.json", digest: "998cbd744dce60cdf59400903c0de950a0f96915cdb0e7f0225b5260882e28e9",
		producer: "v0.1.7-interrupted", commit: "1548dafa30ea3fbf0340b3b32381e885a110ef5e", wantSize: 22, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-004",
		profile:  "portable-minimal",
		file:     "schema-004-portable-minimal.json", digest: "0d02e85c6c6b8a16c53f36d38564e71354e2c66a946f15fb133b2b21def65ef5",
		producer: "schema-004", commit: "f11fe68b2d731e8fd0228352a0b85255d7574abf", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-004",
		profile:  "application-preview-disabled",
		file:     "schema-004-application-disabled.json", digest: "b01cf27856b9103b1c728dc84dc9c6822fc87e8ef2518e94ef402f699c60c127",
		producer: "schema-004", commit: "f11fe68b2d731e8fd0228352a0b85255d7574abf", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-004",
		profile:  "application-preview-gcs",
		file:     "schema-004-application-gcs.json", digest: "097f081e8b41dcdc5f4de3bf8c3c76fd5187c08b5411383da5d64f1f20e279a9",
		producer: "schema-004", commit: "f11fe68b2d731e8fd0228352a0b85255d7574abf", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-005",
		profile:  "portable-minimal",
		file:     "schema-005-v0.2.0-portable-minimal.json", digest: "c342954a139466f8620dff6588f642c957c9cc4f971bcfe383d2f716b31d27d4",
		producer: "v0.2.0", commit: "97e70a84b12de0533b8a7cf4add62ecbf575a0fd", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-005",
		profile:  "application-preview-disabled",
		file:     "schema-005-v0.2.0-application-disabled.json", digest: "ec6c3c617c39fea67d21ed38fb2c05928b353cefa9f5c952f530938ce01db8a0",
		producer: "v0.2.0", commit: "97e70a84b12de0533b8a7cf4add62ecbf575a0fd", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-005",
		profile:  "application-preview-gcs",
		file:     "schema-005-v0.2.0-application-gcs.json", digest: "a72d76f0c24565393d7434cf672cb068281e122418fc5c829fa15371ad6937f9",
		producer: "v0.2.0", commit: "97e70a84b12de0533b8a7cf4add62ecbf575a0fd", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-006",
		profile:  "portable-minimal",
		file:     "schema-006-v0.3.0-portable-minimal.json", digest: "4bed9189a829a4687c310659636f22d74e8babf1a074a2f4136a42c2b934ba2c",
		producer: "v0.3.0", commit: "2d2d49ec9f86e2a247781fd461bcc537459cfbf1", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-006",
		profile:  "application-complete",
		file:     "schema-006-v0.3.2-application-complete.json", digest: "f7007f8be663cc9620ed3f97ff1e32ebc4ddf9f6c1408321ccbbdc69d45ee445",
		producer: "v0.3.2", commit: "8d9715f22737501ae2f7485548ce7993bf804c67", wantSize: 27, wantFiles: 1,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-006",
		profile:  "application-preview-disabled",
		file:     "schema-006-v0.3.0-application-disabled.json", digest: "3daf89025cdedcfb353727ec668e6c7a5b3970abf8ceda60bebcea0b72f507f0",
		producer: "v0.3.0", commit: "2d2d49ec9f86e2a247781fd461bcc537459cfbf1", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-006",
		profile:  "application-preview-gcs",
		file:     "schema-006-v0.3.0-application-gcs.json", digest: "f1bb23118a29d5edfafa1e16d7c28870607b8f92210a0cc85dc46479161beda7",
		producer: "v0.3.0", commit: "2d2d49ec9f86e2a247781fd461bcc537459cfbf1", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-007",
		profile:  "portable-minimal",
		file:     "schema-007-portable-minimal.json", digest: "1a8227ad1293adb53192d2c1079e99e2197cdd12d6df3954395666fe81579731",
		producer: "schema-007", commit: "43171275e93717b1261eeff3b98ecd11b08c9e3f", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-007",
		profile:  "application-complete",
		file:     "schema-007-application-complete.json", digest: "80660b01f183a4680626b0d63ddc591386cb19e120073b9e425e24cc6870a647",
		producer: "schema-007", commit: "43171275e93717b1261eeff3b98ecd11b08c9e3f", wantSize: 27, wantFiles: 1,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-007",
		profile:  "application-preview-disabled",
		file:     "schema-007-application-disabled.json", digest: "10e9d3d9d6f4ddeb34839f1bac21005c6d64fbd36bbbb5371a945f27c722b302",
		producer: "schema-007", commit: "43171275e93717b1261eeff3b98ecd11b08c9e3f", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-007",
		profile:  "application-preview-gcs",
		file:     "schema-007-application-gcs.json", digest: "cb7ebd3c1058347e6ee13d243c9ff6632de9a87a7e1eb7e3c69aa412de25d7bd",
		producer: "schema-007", commit: "43171275e93717b1261eeff3b98ecd11b08c9e3f", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-008",
		profile:  "portable-minimal",
		file:     "schema-008-portable-minimal.json", digest: "d2bf14ccd03e26741310f5604289ba4f90cdea7fd2b697d5e5f8f5396231584a",
		producer: "schema-008", commit: "359ec9fbc9e8020257659c0d91e64372baece1b9", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-008",
		profile:  "application-complete-recovery-residue",
		file:     "schema-008-application-complete-residue.json", digest: "d7872913afa254d853b3a8521b46e3481e1eccbdc8f18940fcd552dac064e9ef",
		producer: "schema-008", commit: "359ec9fbc9e8020257659c0d91e64372baece1b9", wantSize: 27, wantFiles: 1,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-008",
		profile:  "application-preview-disabled",
		file:     "schema-008-application-disabled.json", digest: "c56c6684649dc1c5f7f36bf881877a49f4cea5fb2f98da738af22eade88b7423",
		producer: "schema-008", commit: "359ec9fbc9e8020257659c0d91e64372baece1b9", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-008",
		profile:  "application-preview-gcs",
		file:     "schema-008-application-gcs.json", digest: "62b07aa949bcf325ca7af46f6835ce9986b3f67cd894a243063bb94af0750b87",
		producer: "schema-008", commit: "359ec9fbc9e8020257659c0d91e64372baece1b9", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-009",
		profile:  "portable-minimal",
		file:     "schema-009-portable-minimal.json", digest: "41eaccc3880c07ee9457b1afe44d524a610702ef103b9739461a4a709affc04b",
		producer: "schema-009", commit: "86ad9d8da0e6c45f98d85006f440937557e758dd", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-009",
		profile:  "application-preview-disabled",
		file:     "schema-009-application-disabled.json", digest: "3ffef665e9698baaa11156cc3eab3de37a308aa671532ca83472ed54ee03c1dc",
		producer: "schema-009", commit: "86ad9d8da0e6c45f98d85006f440937557e758dd", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-009",
		profile:  "application-preview-gcs",
		file:     "schema-009-application-gcs.json", digest: "c4cfe1b653f885ca3922f2a7bdf8dfc7016b89923e411eddbc9acf41902a65f9",
		producer: "schema-009", commit: "86ad9d8da0e6c45f98d85006f440937557e758dd", wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-009",
		profile:  "application-complete-production-recovery",
		file:     "schema-009-v0.4.0-application-complete-residue.json", digest: "a2a93ce97fececa99d8244c3d67bac266d104ca42a24f18a0f103b01e01020ea",
		producer: "v0.4.0-production-residue", commit: "642e476ea8c49d9e4e1e9d5672eb63cdf8daff6d", wantSize: 27, wantFiles: 1,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-010",
		profile:  "portable-minimal",
		file:     "schema-010-portable-minimal.json", digest: "0ae63c91e6b5baf95cee87aa5da000d9a51d2b8f7f9d2fdb7dc1edbcf54a98ff",
		producer: "schema-010", commit: "cc5f66c1837baf928eccadaa08dfdb3d86016f44",
		wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-010",
		profile:  "writer-profile-preview-disabled",
		file:     "schema-010-application-disabled.json", digest: "71d735b0d3bf59ce23d36ca99d4dd065bd075634a826f990e4cdfaf6376dc684",
		producer: "schema-010", commit: "cc5f66c1837baf928eccadaa08dfdb3d86016f44",
		wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-010",
		profile:  "writer-profile-preview-gcs",
		file:     "schema-010-application-gcs.json", digest: "108948671ae1a7c87efaf7c0746c41a263d2e1fce4eb8ca798ffe1fd247a40dd",
		producer: "schema-010", commit: "cc5f66c1837baf928eccadaa08dfdb3d86016f44",
		wantSize: 18, wantFiles: 3,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-010",
		profile:  "application-complete",
		file:     "schema-010-application-complete.json", digest: "ac83d7940b5d1f6972655a500c57a574b8103f9e50b2e20f572f1e97b16ceb26",
		producer: "schema-010", commit: "cc5f66c1837baf928eccadaa08dfdb3d86016f44",
		wantSize: 27, wantFiles: 1,
	},
}

var historicalReleases = []string{"v0.1.0", "v0.1.1", "v0.1.2", "v0.1.3", "v0.1.4", "v0.1.5", "v0.1.6", "v0.1.7", "v0.1.8", "v0.1.9", "v0.1.10", "v0.1.11", "v0.1.12", "v0.1.13", "v0.1.14", "v0.2.0", "v0.2.1", "v0.3.0", "v0.3.1", "v0.3.2", "v0.4.0"}

func TestMigrationEveryRegisteredStorageSchemaOpensAndMutatesWithCurrentCode(t *testing.T) {
	history := portable.StorageSchemaHistory()
	fixtureSchemas := make(map[string]struct{}, len(storageSchemaFixtures))
	for familyIndex, family := range storageSchemaFixtures {
		fixtureSchemas[family.schemaID] = struct{}{}
		registered := false
		for _, entry := range history {
			if family.schemaID == entry.ID {
				registered = true
				break
			}
		}
		if !registered {
			t.Fatalf("fixture[%d] schema %s is absent from the production ledger", familyIndex, family.schemaID)
		}
		t.Run(family.schemaID+"/"+family.profile, func(t *testing.T) {
			for topologyIndex, split := range []bool{false, true} {
				topology := "single"
				if split {
					topology = "split"
				}
				t.Run(topology, func(t *testing.T) {
					fixture := loadStorageSchemaFixture(t, family)
					if strings.Contains(family.profile, "application-complete") {
						assertCompleteFixtureContainsPredecessorAuthority(t, fixture)
					}
					stateBackend := objectmemory.New()
					fileBackend := stateBackend
					if split {
						fileBackend = objectmemory.New()
					}
					if err := stateBackend.Import(fixture.StateObjects); err != nil {
						t.Fatal(err)
					}
					if err := fileBackend.Import(fixture.FileObjects); err != nil {
						t.Fatal(err)
					}
					clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
					seed := byte(40 + familyIndex*4 + topologyIndex)
					server := newPortableDataServer(t, fileBackend, clock, seed)
					options := schemaMigrationOptions(stateBackend, clock, seed+20, nil)
					if split {
						options = schemaSplitMigrationOptions(stateBackend, fileBackend, clock, seed+20, nil)
					}
					options.Writer = currentWriterForSchemaFixture(t, fixture)
					engine, err := portable.Open(context.Background(), options)
					if err != nil {
						t.Fatalf("Open(%s %s-backend schema fixture produced by %s) error = %s", family.schemaID, topology, family.producer, migrationErrorChain(err))
					}
					user, err := domain.ParseUserID(fixture.UserID)
					if err != nil {
						t.Fatal(err)
					}
					live, _ := domain.NewScope(user, domain.AreaLive)
					root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
					if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
						t.Fatalf("upgraded %s %s-backend root = %+v, %v; want %d bytes/%d files", family.schemaID, topology, root, err, family.wantSize, family.wantFiles)
					}
					gate, err := engine.GateStatus(context.Background())
					wantEpoch := expectedCurrentGateEpoch(t, history, family.schemaID, fixture)
					if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != wantEpoch {
						t.Fatalf("upgraded %s %s-backend gate = %+v, %v; want open epoch %d", family.schemaID, topology, gate, err, wantEpoch)
					}
					assertNoLegacyUploadRecords(t, stateBackend.Export())
					assertSchema007TerminalAuthorityMigrated(t, engine, fixture)
					if fixture.SemanticOracle != nil {
						assertCompleteMigrationSemanticOracle(t, engine, fixture, clock, seed+40)
					}
					mutationPath := domain.MustParseUserPath("/projects/after-upgrade.txt")
					if fixture.SemanticOracle != nil {
						mutationPath = domain.MustParseUserPath("/after-upgrade.txt")
					}
					uploadPortableFile(t, server.Client(), engine.Files(), live, mutationPath, []byte("ok"))
					after, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
					if err != nil || after.Size != family.wantSize+2 || after.FileCount != family.wantFiles+1 {
						t.Fatalf("post-upgrade %s %s-backend mutation = %+v, %v", family.schemaID, topology, after, err)
					}
				})
			}
		})
	}
	for _, entry := range history {
		if _, found := fixtureSchemas[entry.ID]; !found {
			t.Fatalf("ledger schema %s has no immutable migration fixture", entry.ID)
		}
	}
}

func TestMigrationSchema009ProductionResidueRestoresRealPasskeyAuthentication(t *testing.T) {
	var family storageSchemaFixtureEntry
	for _, candidate := range storageSchemaFixtures {
		if candidate.file == "schema-009-v0.4.0-application-complete-residue.json" {
			family = candidate
			break
		}
	}
	if family.file == "" {
		t.Fatal("schema-009 production-residue fixture is not registered")
	}
	fixture := loadStorageSchemaFixture(t, family)
	assertCompleteFixtureContainsPredecessorAuthority(t, fixture)
	stateBackend, fileBackend := objectmemory.New(), objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(4 * time.Hour))
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 0xe1, nil)
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	engine, err := portable.Open(context.Background(), options)
	if err != nil {
		t.Fatalf("open exact v0.4.0 production residue: %s", migrationErrorChain(err))
	}
	assertCompleteMigrationSemanticOracle(t, engine, fixture, clock, 0xe2)
}

func assertCompleteFixtureContainsPredecessorAuthority(t *testing.T, fixture storageSchemaFixture) {
	t.Helper()
	if fixture.SemanticOracle == nil || len(fixture.SemanticOracle.RequiredKeys) < 12 {
		t.Fatal("complete application fixture is missing its semantic oracle")
	}
	for _, logicalKey := range fixture.SemanticOracle.RequiredKeys {
		binding := []byte(`"logicalKey":"` + logicalKey + `"`)
		found := false
		for _, body := range fixture.StateObjects {
			if bytes.Contains(body, binding) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("predecessor fixture does not contain indexed authority %q", logicalKey)
		}
	}
}

func assertCompleteMigrationSemanticOracle(t *testing.T, engine *portable.Engine, fixture storageSchemaFixture, clock *domain.FixedClock, seed byte) {
	t.Helper()
	oracle := fixture.SemanticOracle
	if oracle == nil || oracle.UserID != fixture.UserID || oracle.DisplayName == "" || oracle.CredentialID == "" || len(oracle.RequiredKeys) < 12 || len(oracle.Authenticator.Credentials) == 0 {
		t.Fatalf("complete application fixture has an incomplete semantic oracle: %+v", oracle)
	}
	ctx := context.Background()
	userID, err := domain.ParseUserID(oracle.UserID)
	if err != nil {
		t.Fatal(err)
	}
	repository := identity.NewRepository(engine)
	profile, _, err := repository.Profile(ctx, userID)
	if err != nil || profile.DisplayName.String() != oracle.DisplayName {
		t.Fatalf("migrated profile = %+v, %v", profile, err)
	}
	account, _, err := repository.Account(ctx, userID)
	if err != nil || account.UserID != userID {
		t.Fatalf("migrated account = %+v, %v", account, err)
	}
	credentials, err := repository.Credentials(ctx, userID)
	if err != nil || len(credentials) != 1 || credentials[0].CredentialID != oracle.CredentialID {
		t.Fatalf("migrated credentials = %+v, %v", credentials, err)
	}
	for _, namespace := range []state.Namespace{
		state.NamespaceSessions, state.NamespaceInvites, state.NamespaceRecoveries, state.NamespaceShares,
		state.NamespaceBootstrap, state.NamespaceRoles, state.NamespacePreferences, state.NamespaceOperations,
	} {
		page, err := engine.List(ctx, state.MustPrefix(namespace), state.PageRequest{Limit: 1000})
		if err != nil || len(page.Items) == 0 {
			t.Fatalf("migrated namespace %s = %d records, %v", namespace, len(page.Items), err)
		}
	}

	const origin = "https://drive.example.test"
	webauthn, err := auth.NewGoWebAuthn("drive.example.test", "EndlessFS", origin)
	if err != nil {
		t.Fatal(err)
	}
	ids := domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 2<<20)))
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, origin, true, secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))))
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewService(repository, webauthn, sessions, ids, clock, identity.NewMutablePolicy(identity.RegistrationPolicy{}), "", origin)
	if err != nil {
		t.Fatal(err)
	}
	start, err := service.StartAuthentication(ctx)
	if err != nil {
		t.Fatal(err)
	}
	options, err := virtualwebauthn.ParseAssertionOptions(string(start.Options))
	if err != nil {
		t.Fatal(err)
	}
	authenticator := oracle.Authenticator
	credentialIndex := -1
	for index := range authenticator.Credentials {
		if base64.RawURLEncoding.EncodeToString(authenticator.Credentials[index].ID) == oracle.CredentialID {
			credentialIndex = index
			break
		}
	}
	if credentialIndex < 0 {
		t.Fatal("semantic oracle authenticator does not contain the persisted credential")
	}
	authenticator.Credentials[credentialIndex].Counter++
	rp := virtualwebauthn.RelyingParty{Name: "EndlessFS", ID: "drive.example.test", Origin: origin}
	assertion := virtualwebauthn.CreateAssertionResponse(rp, authenticator, authenticator.Credentials[credentialIndex], *options)
	issued, err := service.VerifyAuthentication(ctx, start.CeremonyID, start.BrowserBinding, []byte(assertion))
	if err != nil {
		t.Fatalf("real post-migration passkey authentication: %v", err)
	}
	authenticated, err := sessions.Authenticate(ctx, issued.Token.Reveal())
	if err != nil || authenticated.Record.UserID != userID {
		t.Fatalf("post-migration issued session = %+v, %v", authenticated.Record, err)
	}
}

func migrationErrorChain(err error) string {
	parts := make([]string, 0, 4)
	for err != nil {
		parts = append(parts, err.Error())
		err = errors.Unwrap(err)
	}
	return strings.Join(parts, ": ")
}

func assertSchema007TerminalAuthorityMigrated(t *testing.T, engine *portable.Engine, fixture storageSchemaFixture) {
	t.Helper()
	if !strings.Contains(fixture.SourceCommit, "43171275") && fixture.SourceRelease != "schema-007" {
		return
	}
	if fixture.SemanticOracle != nil {
		hasTrashOracle := false
		for _, logicalKey := range fixture.SemanticOracle.RequiredKeys {
			if strings.HasPrefix(logicalKey, string(state.NamespaceTrash)+"/") {
				hasTrashOracle = true
				break
			}
		}
		if !hasTrashOracle {
			return
		}
	}
	ctx := context.Background()
	for keyValue, body := range fixture.StateObjects {
		key := storageformatKey(t, keyValue)
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(body, &generic, storageformat.MaxCanonicalBytes); err != nil {
			continue
		}
		switch generic.Schema {
		case "file-operation-v1":
			var envelope storageformat.Envelope
			var legacy storageformat.FileOperation
			if err := storageformat.DecodeEnvelope(body, key, "file-operation-v1", &envelope, &legacy); err != nil {
				t.Fatal(err)
			}
			owner, err := domain.ParseUserID(legacy.UserID)
			if err != nil {
				t.Fatal(err)
			}
			operation, err := engine.Files().GetOperation(ctx, owner, domain.OperationID(legacy.OperationID))
			if err != nil || operation.ID != domain.OperationID(legacy.OperationID) || operation.StartedAt != legacy.StartedAt || operation.UpdatedAt != legacy.UpdatedAt {
				t.Fatalf("migrated operation %s = %+v, %v", legacy.OperationID, operation, err)
			}
		case "upload-record-v1":
			var envelope storageformat.Envelope
			var legacy storageformat.UploadRecord
			if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &envelope, &legacy); err != nil {
				t.Fatal(err)
			}
			owner, err := domain.ParseUserID(legacy.UserID)
			if err != nil {
				t.Fatal(err)
			}
			area := domain.AreaLive
			if legacy.Area == "trash" {
				area = domain.AreaTrash
			}
			scope, _ := domain.NewScope(owner, area)
			status, err := engine.Files().UploadStatus(ctx, scope, domain.UploadID(legacy.UploadID))
			if err != nil || status.UploadID != domain.UploadID(legacy.UploadID) || legacy.State == storageformat.UploadCompleted && status.State != domain.UploadStateCompleted || legacy.State == storageformat.UploadAborted && status.State != domain.UploadStateAborted {
				t.Fatalf("migrated upload %s = %+v, %v; legacy=%+v", legacy.UploadID, status, err, legacy)
			}
		}
	}
	owner, err := domain.ParseUserID(fixture.UserID)
	if err != nil {
		t.Fatal(err)
	}
	live, _ := domain.NewScope(owner, domain.AreaLive)
	trash, _ := domain.NewScope(owner, domain.AreaTrash)
	trashPage, err := engine.Files().ListTrash(ctx, owner, domain.TrashListRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range trashPage.Items {
		if item.OriginalPath != domain.MustParseUserPath("/trash-me.txt") {
			continue
		}
		replayed, err := engine.Files().Move(ctx, live, trash, domain.MoveRequest{
			Source: item.OriginalPath, Destination: item.TrashedPath, Conflict: domain.ConflictFail,
			ExpectedSource: item.OriginalVersion, IdempotencyKey: "fixture-trash",
		})
		if err != nil || replayed.State != domain.OperationSucceeded {
			t.Fatalf("migrated schema-007 idempotent move replay = %+v, %v", replayed, err)
		}
		if _, err := engine.Files().Move(ctx, live, trash, domain.MoveRequest{
			Source: item.OriginalPath, Destination: domain.MustParseUserPath("/different"), Conflict: domain.ConflictFail,
			ExpectedSource: item.OriginalVersion, IdempotencyKey: "fixture-trash",
		}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("changed migrated schema-007 idempotent move error = %v", err)
		}
		return
	}
	trashRoot, statErr := engine.Files().Stat(ctx, trash, domain.MustParseUserPath("/"))
	t.Fatalf("schema-007 fixture trash entry was not migrated; page=%+v root=%+v rootErr=%v", trashPage, trashRoot, statErr)
}

func TestEveryHistoricalReleaseMapsToRegisteredStorageSchemaFixture(t *testing.T) {
	fixtureSchemas := make(map[string]struct{}, len(storageSchemaFixtures))
	for _, family := range storageSchemaFixtures {
		fixtureSchemas[family.schemaID] = struct{}{}
	}
	for _, release := range historicalReleases {
		schemaID, found := portable.StorageSchemaForRelease(release)
		if !found {
			t.Fatalf("historical release %s is outside every ledger validity range", release)
		}
		if _, registered := fixtureSchemas[schemaID]; !registered {
			t.Fatalf("historical release %s maps to schema %s without an immutable fixture", release, schemaID)
		}
	}
	if candidate := os.Getenv(migrationCandidateReleaseEnvironment); candidate != "" {
		schemaID, found := portable.StorageSchemaForRelease(candidate)
		if !found {
			t.Fatalf("release candidate %s is outside every declared storage-schema validity range", candidate)
		}
		if _, registered := fixtureSchemas[schemaID]; !registered {
			t.Fatalf("release candidate %s maps to schema %s without an immutable fixture", candidate, schemaID)
		}
	}
}

func TestMigrationCheckpointInventoryResumesFromBoundedPagesWithoutReadingFileBodies(t *testing.T) {
	family := storageSchemaFixtures[1]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	for index := range 1_200 {
		key := storageformat.BlobKey(fixture.UserID, "restartable-blob-"+fmt.Sprint(index))
		if _, err := fileBackend.Put(context.Background(), key, []byte{byte(index + 1)}, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}

	interrupted := &interruptingInventoryBackend{Backend: fileBackend, failAfter: 1}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 214, nil)
	options.FileBackend = interrupted
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	if _, err := portable.Open(context.Background(), options); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted large-inventory migration error = %v; want unavailable", err)
	}
	interrupted.disableFailure()
	if _, err := portable.Open(context.Background(), options); err != nil {
		t.Fatalf("resumed large-inventory migration error = %v", err)
	}

	if got := interrupted.totalBodyReads(); got != 0 {
		t.Fatalf("file body reads across interrupted migration = %d; want 0", got)
	}
	if got := interrupted.totalAttempts(); got != 22 {
		t.Fatalf("file metadata list calls across interrupted schema-002-to-010 migration = %d; want measured restart ratchet 22", got)
	}
	progressRecords := 0
	inventoryPages := 0
	for key := range stateBackend.Export() {
		if strings.Contains(key, "/checkpoints/") && strings.Contains(key, "/work/") {
			progressRecords++
		}
		if strings.Contains(key, "/checkpoints/") && strings.Contains(key, "/inventory/") {
			inventoryPages++
		}
	}
	if progressRecords != 0 || inventoryPages == 0 {
		t.Fatalf("checkpoint artifacts = %d work records, %d bounded pages; want 0 work and pages", progressRecords, inventoryPages)
	}
}

func TestMigrationCheckpointInventoryRejectsForgedRestartPage(t *testing.T) {
	family := storageSchemaFixtures[1]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	for index := range 1_200 {
		key := storageformat.BlobKey(fixture.UserID, "forged-page-blob-"+fmt.Sprint(index))
		if _, err := fileBackend.Put(context.Background(), key, []byte{byte(index + 1)}, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	interrupted := &interruptingInventoryBackend{Backend: fileBackend, failAfter: 1}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 217, nil)
	options.FileBackend = interrupted
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	if _, err := portable.Open(context.Background(), options); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted migration error = %v; want unavailable", err)
	}

	var progressKey objectstore.Key
	for keyValue := range stateBackend.Export() {
		if strings.Contains(keyValue, "/checkpoints/") && strings.Contains(keyValue, "/inventory/") {
			progressKey = storageformatKey(t, keyValue)
			break
		}
	}
	if !progressKey.Valid() {
		t.Fatal("interrupted checkpoint inventory did not persist a bounded page")
	}
	object, err := stateBackend.Get(context.Background(), progressKey)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var progress storageformat.CheckpointInventoryPage
	if err := storageformat.DecodeEnvelope(object.Body, progressKey, "checkpoint-inventory-page-v2", &envelope, &progress); err != nil {
		t.Fatal(err)
	}
	progress.Entries[0].Object.MD5 = objectstore.FingerprintFor([]byte("forged")).MD5
	forged := mustEnvelope(t, "checkpoint-inventory-page-v2", progressKey, envelope.Revision+1, progress)
	if _, err := stateBackend.Put(context.Background(), progressKey, forged, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	interrupted.disableFailure()
	if _, err := portable.Open(context.Background(), options); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("forged checkpoint progress error = %v; want precondition failed", err)
	}
}

func TestMigrationCheckpointInventoryRejectsSameSizeBodyChangeAfterProgress(t *testing.T) {
	family := storageSchemaFixtures[1]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	for index := range 1_200 {
		key := storageformat.BlobKey(fixture.UserID, "integrity-blob-"+fmt.Sprint(index))
		if _, err := fileBackend.Put(context.Background(), key, []byte("body-"+fmt.Sprint(index)), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	interrupted := &interruptingInventoryBackend{Backend: fileBackend, failAfter: 1}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 218, nil)
	options.FileBackend = interrupted
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	if _, err := portable.Open(context.Background(), options); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted migration error = %v; want unavailable", err)
	}

	var progressed storageformat.CheckpointInventoryEntry
	for keyValue, body := range stateBackend.Export() {
		if !strings.Contains(keyValue, "/checkpoints/") || !strings.Contains(keyValue, "/inventory/") {
			continue
		}
		key := storageformatKey(t, keyValue)
		var envelope storageformat.Envelope
		var page storageformat.CheckpointInventoryPage
		if err := storageformat.DecodeEnvelope(body, key, "checkpoint-inventory-page-v2", &envelope, &page); err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Entries {
			if entry.FileData && entry.Object.Size > 0 {
				progressed = entry
				break
			}
		}
		if progressed.Object.Key != "" {
			break
		}
	}
	if progressed.Object.Key == "" || !progressed.FileData || progressed.Object.Size == 0 {
		t.Fatal("interrupted checkpoint has no completed file progress")
	}
	bodyKey := storageformatKey(t, progressed.Object.Key)
	object, err := fileBackend.Get(context.Background(), bodyKey)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), object.Body...)
	changed[0] ^= 0xff
	if _, err := fileBackend.Put(context.Background(), bodyKey, changed, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	interrupted.disableFailure()
	if _, err := portable.Open(context.Background(), options); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("same-size changed checkpoint object error = %v; want precondition failed", err)
	}
}

func TestMigrationDoesNotReadFileBodiesAgainImmediatelyBeforeOpeningWrites(t *testing.T) {
	family := storageSchemaFixtures[1]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	counting := &interruptingInventoryBackend{Backend: fileBackend, failAfter: -1}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 215, nil)
	options.FileBackend = counting
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	if _, err := portable.Open(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if got := counting.totalBodyReads(); got != 0 {
		t.Fatalf("file body reads during migration = %d; want 0", got)
	}
	if got := counting.totalAttempts(); got != 11 {
		t.Fatalf("file metadata listing calls across schema-002-to-010 checkpoint suffix = %d; want measured ratchet 11", got)
	}
}

func TestMigrationReportsStageAndProviderIndependentInventoryProgress(t *testing.T) {
	family := storageSchemaFixtures[1]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	events := make([]portable.MigrationProgress, 0)
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 216, nil)
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	options.MigrationObserver = func(progress portable.MigrationProgress) {
		events = append(events, progress)
	}
	if _, err := portable.Open(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	wantStages := map[string]bool{
		portable.MigrationStageStarted:             false,
		portable.MigrationStageGateClosed:          false,
		portable.MigrationStageDirectoriesVerified: false,
		portable.MigrationStageCheckpointInventory: false,
		portable.MigrationStageCheckpointCreated:   false,
		portable.MigrationStageComplete:            false,
	}
	for _, event := range events {
		if _, found := wantStages[event.Stage]; found {
			wantStages[event.Stage] = true
		}
		if event.CompletedObjects < 0 || (event.TotalObjects != 0 && event.TotalObjects < event.CompletedObjects) || event.CompletedBytes < 0 || (event.TotalBytes != 0 && event.TotalBytes < event.CompletedBytes) {
			t.Fatalf("invalid migration progress event: %+v", event)
		}
	}
	for stage, observed := range wantStages {
		if !observed {
			t.Fatalf("migration stage %q was not observed in %+v", stage, events)
		}
	}
}

type interruptingInventoryBackend struct {
	objectstore.Backend
	mu        sync.Mutex
	failAfter int
	failed    bool
	listCalls int
	bodyReads int
}

func (backend *interruptingInventoryBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	backend.mu.Lock()
	backend.bodyReads++
	backend.mu.Unlock()
	return backend.Backend.Get(ctx, key)
}

func (backend *interruptingInventoryBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	backend.mu.Lock()
	backend.bodyReads++
	backend.mu.Unlock()
	return backend.Backend.Open(ctx, key)
}

func (backend *interruptingInventoryBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	backend.mu.Lock()
	backend.listCalls++
	if backend.failAfter >= 0 && !backend.failed && backend.listCalls > backend.failAfter {
		backend.failed = true
		backend.mu.Unlock()
		return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "injected checkpoint metadata-list interruption")
	}
	backend.mu.Unlock()
	return backend.Backend.List(ctx, request)
}

func (backend *interruptingInventoryBackend) disableFailure() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.failAfter = -1
}

func (backend *interruptingInventoryBackend) totalAttempts() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.listCalls
}

func (backend *interruptingInventoryBackend) totalBodyReads() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.bodyReads
}

func TestMigrationEveryLedgerEdgeResumesAfterEveryDurableBoundary(t *testing.T) {
	boundaries := []string{
		portable.StepMigrationAfterDetection,
		portable.StepMigrationAfterGateClosed,
		portable.StepMigrationAfterDirectoryPrerequisites,
		portable.StepMigrationAfterDirectoryRoot,
		portable.StepMigrationAfterDirectories,
		portable.StepMigrationAfterWriterSet,
		portable.StepMigrationAfterSuperblock,
		portable.StepMigrationAfterGateBinding,
		portable.StepMigrationAfterCheckpoint,
	}
	history := portable.StorageSchemaHistory()
	for edgeIndex, entry := range history[:len(history)-1] {
		families := storageSchemaFixturesFor(entry.ID)
		edgeBoundaries := boundaries
		if entry.MigrationID == "schema-004-to-005" || entry.MigrationID == "schema-005-to-006" || entry.MigrationID == "schema-006-to-007" || entry.MigrationID == "schema-007-to-008" {
			edgeBoundaries = []string{
				portable.StepMigrationAfterDetection,
				portable.StepMigrationAfterGateClosed,
				portable.StepMigrationAfterDirectories,
				portable.StepMigrationAfterWriterSet,
				portable.StepMigrationAfterSuperblock,
				portable.StepMigrationAfterGateBinding,
				portable.StepMigrationAfterCheckpoint,
			}
		}
		for familyIndex, family := range families {
			for boundaryIndex, boundary := range edgeBoundaries {
				t.Run(entry.MigrationID+"/"+family.profile+"/"+boundary, func(t *testing.T) {
					fixture := loadStorageSchemaFixture(t, family)
					stateBackend := objectmemory.New()
					fileBackend := objectmemory.New()
					if err := stateBackend.Import(fixture.StateObjects); err != nil {
						t.Fatal(err)
					}
					if err := fileBackend.Import(fixture.FileObjects); err != nil {
						t.Fatal(err)
					}
					clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
					crasher := &stepFailure{step: portable.MigrationStepName(entry.MigrationID, boundary)}
					seed := byte(100 + edgeIndex*48 + familyIndex*12 + boundaryIndex)
					options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, seed, crasher)
					options.Writer = currentWriterForSchemaFixture(t, fixture)
					if _, err := portable.Open(context.Background(), options); !errors.Is(err, domain.ErrUnavailable) {
						t.Fatalf("interrupted %s at %s error = %v; want unavailable", entry.MigrationID, boundary, err)
					}
					options = schemaSplitMigrationOptions(stateBackend, fileBackend, clock, seed+64, nil)
					options.Writer = currentWriterForSchemaFixture(t, fixture)
					engine, err := portable.Open(context.Background(), options)
					if err != nil {
						t.Fatalf("resume %s after %s error = %v", entry.MigrationID, boundary, err)
					}
					user, err := domain.ParseUserID(fixture.UserID)
					if err != nil {
						t.Fatal(err)
					}
					live, _ := domain.NewScope(user, domain.AreaLive)
					root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
					if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
						t.Fatalf("resumed %s root = %+v, %v; want %d bytes/%d files", entry.MigrationID, root, err, family.wantSize, family.wantFiles)
					}
					gate, err := engine.GateStatus(context.Background())
					wantEpoch := expectedCurrentGateEpoch(t, history, family.schemaID, fixture)
					if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != wantEpoch {
						t.Fatalf("resumed %s gate = %+v, %v; want open epoch %d", entry.MigrationID, gate, err, wantEpoch)
					}
				})
			}
		}
	}
}

func expectedCurrentGateEpoch(t *testing.T, history []portable.StorageSchemaHistoryEntry, schemaID string, fixture storageSchemaFixture) uint64 {
	t.Helper()
	gateBody, found := fixture.StateObjects[storageformat.WriteGateKey().String()]
	if !found {
		t.Fatalf("schema %s fixture has no canonical write gate", schemaID)
	}
	var envelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(gateBody, storageformat.WriteGateKey(), "write-gate-v1", &envelope, &gate); err != nil {
		t.Fatalf("decode schema %s fixture write gate: %v", schemaID, err)
	}
	for index, entry := range history {
		if entry.ID == schemaID {
			return gate.Epoch + uint64(len(history)-index-1)
		}
	}
	t.Fatalf("schema %s is absent from the storage ledger", schemaID)
	return 0
}

func storageSchemaFixturesFor(schemaID string) []storageSchemaFixtureEntry {
	fixtures := make([]storageSchemaFixtureEntry, 0)
	for _, fixture := range storageSchemaFixtures {
		if fixture.schemaID == schemaID {
			fixtures = append(fixtures, fixture)
		}
	}
	return fixtures
}

func TestMigrationOldestSchemaTraversesLedgerEdgesInOrder(t *testing.T) {
	history := portable.StorageSchemaHistory()
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(history)-1)
	for _, entry := range history[:len(history)-1] {
		want = append(want, portable.MigrationStepName(entry.MigrationID, portable.StepMigrationAfterDetection))
	}
	got := make([]string, 0, len(want))
	scheduler := portable.SchedulerFunc(func(_ context.Context, step string) error {
		for _, expected := range want {
			if step == expected {
				got = append(got, step)
				break
			}
		}
		return nil
	})
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 180, scheduler)); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("observed migration-edge starts = %v; want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("observed migration-edge starts = %v; want %v", got, want)
		}
	}
}

func TestMigrationSchema001CASLoserAcceptsValidatedSchema003Winner(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	paused := make(chan struct{})
	resume := make(chan struct{})
	pausedOnce := false
	scheduler := portable.SchedulerFunc(func(ctx context.Context, step string) error {
		if step != portable.MigrationStepName("schema-001-to-002", portable.StepMigrationAfterDirectoryPrerequisites) || pausedOnce {
			return nil
		}
		pausedOnce = true
		close(paused)
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	firstResult := make(chan error, 1)
	go func() {
		_, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 181, scheduler))
		firstResult <- err
	}()
	<-paused
	winner, winnerErr := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 182, nil))
	close(resume)
	if winnerErr != nil {
		t.Fatalf("winning migration error = %v", winnerErr)
	}
	if err := <-firstResult; err != nil {
		t.Fatalf("schema-001 CAS loser rejected validated schema-003 winner: %v", err)
	}
	user, _ := domain.ParseUserID(fixture.UserID)
	live, _ := domain.NewScope(user, domain.AreaLive)
	root, err := winner.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
	if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
		t.Fatalf("winning migration root = %+v, %v", root, err)
	}
}

func TestMigrationLaggingReplicaAcceptsCompletedWinnerAfterSourceCollection(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	laggingBackend := &pauseOnceGetBackend{
		Backend: stateBackend,
		match:   "/manifests/",
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
	laggingOptions := schemaSplitMigrationOptions(laggingBackend, fileBackend, clock, 183, nil)
	laggingOptions.Writer = currentWriterForSchemaFixture(t, fixture)
	laggingResult := make(chan error, 1)
	go func() {
		_, err := portable.Open(context.Background(), laggingOptions)
		laggingResult <- err
	}()

	<-laggingBackend.reached
	t.Cleanup(laggingBackend.resume)
	winnerOptions := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 184, nil)
	winnerOptions.Writer = currentWriterForSchemaFixture(t, fixture)
	if _, err := portable.Open(context.Background(), winnerOptions); err != nil {
		t.Fatalf("winning migration error = %v", err)
	}
	supersededKey := laggingBackend.pausedKey()
	assertMigrationManifestSuperseded(t, stateBackend, supersededKey)
	superseded, err := stateBackend.Get(context.Background(), supersededKey)
	if err == nil {
		if err := stateBackend.Delete(context.Background(), supersededKey, objectstore.DeleteCondition{Version: superseded.Version}); err != nil {
			t.Fatalf("collect superseded source manifest: %v", err)
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("read superseded source manifest: %v", err)
	}
	laggingBackend.resume()
	if err := <-laggingResult; err != nil {
		t.Fatalf("lagging migration rejected completed winner after source collection: %v", err)
	}
}

func TestMigrationMissingSourceWithoutCompletedWinnerFailsClosed(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	backend := &pauseOnceGetBackend{
		Backend: stateBackend,
		match:   "/manifests/",
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
	options := schemaSplitMigrationOptions(backend, fileBackend, clock, 185, nil)
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	result := make(chan error, 1)
	go func() {
		_, err := portable.Open(context.Background(), options)
		result <- err
	}()

	<-backend.reached
	t.Cleanup(backend.resume)
	missingKey := backend.pausedKey()
	missing, err := stateBackend.Get(context.Background(), missingKey)
	if err != nil {
		t.Fatalf("read source manifest: %v", err)
	}
	if err := stateBackend.Delete(context.Background(), missingKey, objectstore.DeleteCondition{Version: missing.Version}); err != nil {
		t.Fatalf("remove source manifest: %v", err)
	}
	backend.resume()
	if err := <-result; !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("incomplete migration with missing source error = %v; want not found", err)
	}
}

type pauseOnceGetBackend struct {
	objectstore.Backend
	match      string
	reached    chan struct{}
	release    chan struct{}
	mu         sync.Mutex
	resumeOnce sync.Once
	paused     bool
	key        objectstore.Key
}

func (backend *pauseOnceGetBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	backend.mu.Lock()
	shouldPause := !backend.paused && strings.Contains(key.String(), backend.match)
	if shouldPause {
		backend.paused = true
		backend.key = key
		close(backend.reached)
	}
	backend.mu.Unlock()
	if shouldPause {
		select {
		case <-backend.release:
		case <-ctx.Done():
			return objectstore.Object{}, ctx.Err()
		}
	}
	return backend.Backend.Get(ctx, key)
}

func (backend *pauseOnceGetBackend) pausedKey() objectstore.Key {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.key
}

func (backend *pauseOnceGetBackend) resume() {
	backend.resumeOnce.Do(func() { close(backend.release) })
}

func assertMigrationManifestSuperseded(t *testing.T, backend objectstore.Backend, manifestKey objectstore.Key) {
	t.Helper()
	parts := strings.Split(manifestKey.String(), "/")
	if len(parts) != 9 || parts[0] != "endlessfs" || parts[1] != "v1" || parts[2] != "fs" || parts[5] != "dirs" || parts[7] != "manifests" || !strings.HasSuffix(parts[8], ".json") {
		t.Fatalf("unexpected migration manifest key %q", manifestKey)
	}
	rootKey, err := objectstore.ParseKey(strings.Join(parts[:7], "/") + "/directory.json")
	if err != nil {
		t.Fatalf("construct migrated directory root key: %v", err)
	}
	rootObject, err := backend.Get(context.Background(), rootKey)
	if err != nil {
		t.Fatalf("read migrated directory root: %v", err)
	}
	var envelope storageformat.Envelope
	var root storageformat.DirectoryRoot
	if err := storageformat.DecodeEnvelope(rootObject.Body, rootKey, "directory-root-v1", &envelope, &root); err != nil {
		t.Fatalf("decode migrated directory root: %v", err)
	}
	userID, area, directoryID, matched, err := storageformat.ParseDirectoryRootKey(rootKey)
	if err != nil || !matched {
		t.Fatalf("parse migrated directory root key: %v", err)
	}
	if storageformat.DirectoryManifestKey(userID, area, directoryID, root.ManifestID) == manifestKey {
		t.Fatalf("migration source manifest %q remains authoritative", manifestKey)
	}
}

func TestSchema001MigrationResumesAfterUploadRecordUpgrade(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	initialCurrent, initialSchema001 := countUploadRecordSchemas(t, fixture.StateObjects)
	if initialCurrent != 0 || initialSchema001 < 2 {
		t.Fatalf("schema-001 fixture upload schemas = %d current/%d schema-001; want 0/at least 2", initialCurrent, initialSchema001)
	}
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	crasher := &stepFailure{step: portable.MigrationStepName("schema-001-to-002", portable.StepMigrationAfterUploadRecord)}
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 70, crasher)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted schema-001 migration error = %v; want unavailable", err)
	}
	current, schema001 := countUploadRecordSchemas(t, stateBackend.Export())
	if current != 1 || schema001 != initialSchema001-1 {
		t.Fatalf("interrupted schema-001 migration upload schemas = %d current/%d schema-001; want 1/%d", current, schema001, initialSchema001-1)
	}
	engine, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 71, nil))
	if err != nil {
		t.Fatalf("resumed schema-001 migration error = %v", err)
	}
	user, _ := domain.ParseUserID(fixture.UserID)
	live, _ := domain.NewScope(user, domain.AreaLive)
	root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
	if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
		t.Fatalf("resumed schema-001 migration root = %+v, %v", root, err)
	}
}

func TestEightReplicasConcurrentlyMigrateSchema001Fixture(t *testing.T) {
	for familyIndex, family := range storageSchemaFixturesFor("endlessfs-portable-v1/schema-001") {
		t.Run(family.profile, func(t *testing.T) {
			fixture := loadStorageSchemaFixture(t, family)
			stateBackend := objectmemory.New()
			fileBackend := objectmemory.New()
			if err := stateBackend.Import(fixture.StateObjects); err != nil {
				t.Fatal(err)
			}
			if err := fileBackend.Import(fixture.FileObjects); err != nil {
				t.Fatal(err)
			}
			clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
			writer := currentWriterForSchemaFixture(t, fixture)
			const replicas = 8
			barrier := newAggregateBarrier(replicas)
			engines := make([]*portable.Engine, replicas)
			errorsFound := make([]error, replicas)
			var wait sync.WaitGroup
			for index := range replicas {
				wait.Add(1)
				go func() {
					defer wait.Done()
					scheduler := &aggregateOneShotScheduler{step: portable.MigrationStepName("schema-001-to-002", portable.StepMigrationAfterDetection), barrier: barrier, enabled: true}
					options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, byte(80+familyIndex*16+index), scheduler)
					options.Writer = writer
					engines[index], errorsFound[index] = portable.Open(context.Background(), options)
				}()
			}
			wait.Wait()
			for index, err := range errorsFound {
				if err != nil {
					t.Errorf("schema-001 migration replica %d error = %v", index, err)
				}
			}
			if t.Failed() {
				t.FailNow()
			}
			assertAllUploadRecordsUseCurrentSchema(t, stateBackend.Export())
			user, _ := domain.ParseUserID(fixture.UserID)
			live, _ := domain.NewScope(user, domain.AreaLive)
			root, err := engines[replicas-1].Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
			if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
				t.Fatalf("concurrently migrated release root = %+v, %v", root, err)
			}
		})
	}
}

func TestSchema001MigrationRejectsCorruptUploadRecord(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	corruptHistoricalUploadRecord(t, fixture.StateObjects)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 72, nil)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt historical upload migration error = %v; want invalid", err)
	}
	assertRecursiveFeatureInactive(t, stateBackend.Export())
}

func TestSchema002MigrationRejectsInconsistentPersistedAggregate(t *testing.T) {
	family := storageSchemaFixtures[1]
	fixture := loadStorageSchemaFixture(t, family)
	mutateSchemaFixturePage(t, fixture.StateObjects, func(page *storageformat.DirectoryPage) bool {
		for index := range page.Entries {
			if page.Entries[index].Kind != domain.EntryFile {
				continue
			}
			page.Entries[index].Size++
			page.Entries[index].LogicalVersion = entryLogicalVersion(t, page.Entries[index])
			return true
		}
		return false
	})
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 73, nil)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("schema-002 inconsistent aggregate migration error = %v; want invalid", err)
	}
}

func loadStorageSchemaFixture(t *testing.T, family storageSchemaFixtureEntry) storageSchemaFixture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "migrations", family.file))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if got := hex.EncodeToString(digest[:]); got != family.digest {
		t.Fatalf("historical fixture %s digest = %s; want immutable digest %s", family.file, got, family.digest)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var fixture storageSchemaFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("historical fixture trailing JSON error = %v; want EOF", err)
	}
	if fixture.SchemaVersion != 1 || fixture.SourceRelease != family.producer || fixture.SourceCommit != family.commit || fixture.CreatedAt.IsZero() || fixture.UserID == "" || len(fixture.StateObjects) == 0 || len(fixture.FileObjects) == 0 {
		t.Fatalf("historical fixture metadata is invalid: %+v", fixture)
	}
	return fixture
}

func currentWriterForSchemaFixture(t *testing.T, fixture storageSchemaFixture) portable.WriterConfiguration {
	t.Helper()
	writerKey := storageformat.WriterSetKey()
	var envelope storageformat.Envelope
	var writer storageformat.WriterSet
	if err := storageformat.DecodeEnvelope(fixture.StateObjects[writerKey.String()], writerKey, "writer-set-v1", &envelope, &writer); err != nil {
		t.Fatal(err)
	}
	return portable.WriterConfiguration{
		WriterSetID: writer.WriterSetID, ConfigurationDigest: writer.ConfigurationDigest,
		KeyringIdentifiers: append([]string(nil), writer.KeyringIdentifiers...),
		RequiredFeatures:   append([]string(nil), writer.RequiredFeatures...),
	}
}

type schema001FixtureUploadRecord struct {
	SchemaVersion   int                       `json:"schemaVersion"`
	UploadID        string                    `json:"uploadID"`
	UserID          string                    `json:"userID"`
	Area            string                    `json:"area"`
	RequestedPath   string                    `json:"requestedPath"`
	ResolvedPath    string                    `json:"resolvedPath"`
	StagingKey      string                    `json:"stagingKey"`
	BackendKind     string                    `json:"backendKind,omitempty"`
	LeaseKey        string                    `json:"leaseKey,omitempty"`
	Size            int64                     `json:"size"`
	MediaType       string                    `json:"mediaType"`
	Conflict        domain.ConflictMode       `json:"conflict"`
	ExpectedVersion domain.Version            `json:"expectedVersion,omitempty"`
	TargetExisted   bool                      `json:"targetExisted"`
	Resumable       bool                      `json:"resumable"`
	State           storageformat.UploadState `json:"state"`
	CreatedAt       time.Time                 `json:"createdAt"`
	ExpiresAt       time.Time                 `json:"expiresAt"`
}

func corruptHistoricalUploadRecord(t *testing.T, objects map[string][]byte) {
	t.Helper()
	for keyValue, body := range objects {
		key := storageformatKey(t, keyValue)
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(body, &generic, storageformat.MaxCanonicalBytes); err != nil || generic.Schema != "upload-record-v1" {
			continue
		}
		var envelope storageformat.Envelope
		var record schema001FixtureUploadRecord
		if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &envelope, &record); err != nil {
			t.Fatal(err)
		}
		record.BackendKind = "invalid_backend"
		objects[keyValue] = mustEnvelope(t, "upload-record-v1", key, envelope.Revision+1, record)
		return
	}
	t.Fatal("historical fixture has no upload record")
}

func assertNoLegacyUploadRecords(t *testing.T, objects map[string][]byte) {
	t.Helper()
	_, schema001 := countUploadRecordSchemas(t, objects)
	if schema001 != 0 {
		t.Fatalf("schema migration fixture retains %d schema-001 upload records; want none", schema001)
	}
}

func assertAllUploadRecordsUseCurrentSchema(t *testing.T, objects map[string][]byte) {
	t.Helper()
	current, schema001 := countUploadRecordSchemas(t, objects)
	if current < 2 || schema001 != 0 {
		t.Fatalf("schema migration fixture exposed %d current/%d schema-001 upload records; want at least 2/0", current, schema001)
	}
}

func countUploadRecordSchemas(t *testing.T, objects map[string][]byte) (int, int) {
	t.Helper()
	currentCount := 0
	schema001Count := 0
	for keyValue, body := range objects {
		key := storageformatKey(t, keyValue)
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(body, &generic, storageformat.MaxCanonicalBytes); err != nil || generic.Schema != "upload-record-v1" {
			continue
		}
		var envelope storageformat.Envelope
		var record storageformat.UploadRecord
		if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &envelope, &record); err == nil {
			if record.CompletionOperationID == "" {
				t.Fatalf("current upload record %s lacks completion operation ID", keyValue)
			}
			currentCount++
			continue
		}
		var schema001Envelope storageformat.Envelope
		var schema001Record schema001FixtureUploadRecord
		if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &schema001Envelope, &schema001Record); err != nil {
			t.Fatalf("upload record %s is neither the current nor registered historical schema: %v", keyValue, err)
		}
		schema001Count++
	}
	return currentCount, schema001Count
}
