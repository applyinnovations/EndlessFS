package storageformat

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// ConsistencyDomainKind identifies one independently mutable authority. The
// values are canonical format identifiers, not provider resources.
type ConsistencyDomainKind string

const (
	DomainNamespace    ConsistencyDomainKind = "namespace"
	DomainOwnerControl ConsistencyDomainKind = "owner-control"
	DomainAdmin        ConsistencyDomainKind = "administration"
	DomainCapability   ConsistencyDomainKind = "capability"
	DomainShare        ConsistencyDomainKind = "share"
	DomainIdentity     ConsistencyDomainKind = "owner-identity"
	DomainOwnerJobs    ConsistencyDomainKind = "owner-jobs"
)

type ProjectionKind string

const (
	ProjectionDuplicates ProjectionKind = "duplicates"
	ProjectionAdminUsers ProjectionKind = "admin-users"
	ProjectionAccounting ProjectionKind = "accounting"
	ProjectionSearch     ProjectionKind = "search"
	ProjectionModified   ProjectionKind = "namespace-modified"
	ProjectionSize       ProjectionKind = "namespace-size"
	ProjectionEntryKind  ProjectionKind = "namespace-kind"
)

// DomainTreeRoot is a bounded descriptor for an immutable ordered tree.
type DomainTreeRoot struct {
	Digest     string `json:"digest,omitempty"`
	Level      int    `json:"level"`
	EntryCount uint64 `json:"entryCount"`
	ByteCount  uint64 `json:"byteCount,omitempty"`
}

type DomainEntry struct {
	Key            string `json:"key"`
	Value          []byte `json:"value"`
	LogicalVersion string `json:"logicalVersion"`
}

type DomainPageChild struct {
	FirstKey   string `json:"firstKey"`
	LastKey    string `json:"lastKey"`
	Digest     string `json:"digest"`
	Level      int    `json:"level"`
	EntryCount uint64 `json:"entryCount"`
	ByteCount  uint64 `json:"byteCount,omitempty"`
}

// DomainPage is immutable and content addressed. Leaves embed bounded record
// values so a record read needs no second value-object lookup.
type DomainPage struct {
	SchemaVersion int                   `json:"schemaVersion"`
	DomainID      string                `json:"domainID"`
	Kind          ConsistencyDomainKind `json:"kind"`
	Level         int                   `json:"level"`
	Entries       []DomainEntry         `json:"entries,omitempty"`
	Children      []DomainPageChild     `json:"children,omitempty"`
}

type DomainChange struct {
	Key            string `json:"key"`
	Delete         bool   `json:"delete,omitempty"`
	Value          []byte `json:"value,omitempty"`
	LogicalVersion string `json:"logicalVersion,omitempty"`
}

type DomainDelta struct {
	MutationID  string         `json:"mutationID"`
	Fingerprint string         `json:"fingerprint"`
	Revision    uint64         `json:"revision"`
	RetainUntil time.Time      `json:"retainUntil"`
	Changes     []DomainChange `json:"changes"`
	Result      []byte         `json:"result,omitempty"`
}

// DomainOutcome is the durable replay result for one published mutation. It
// lives in the domain's immutable outcome tree after the corresponding delta
// is compacted. Keeping outcomes inside the authority root removes the need
// for a second mutable claim object and makes lost-success recovery a normal
// authenticated head/tree read.
type DomainOutcome struct {
	MutationID  string    `json:"mutationID"`
	Fingerprint string    `json:"fingerprint"`
	Revision    uint64    `json:"revision"`
	RetainUntil time.Time `json:"retainUntil"`
	Result      []byte    `json:"result,omitempty"`
}

// DomainHead is the sole visibility root for one consistency domain. Its
// canonical envelope revision is the portable concurrency version; Revision
// here binds deltas, claims, projections, and batch read proofs.
type DomainHead struct {
	SchemaVersion int                   `json:"schemaVersion"`
	DomainID      string                `json:"domainID"`
	Kind          ConsistencyDomainKind `json:"kind"`
	Registered    bool                  `json:"registered"`
	Revision      uint64                `json:"revision"`
	Frozen        bool                  `json:"frozen,omitempty"`
	FreezeEpoch   uint64                `json:"freezeEpoch,omitempty"`
	BaseRevision  uint64                `json:"baseRevision,omitempty"`
	Base          DomainTreeRoot        `json:"base"`
	Outcomes      DomainTreeRoot        `json:"outcomes"`
	OutcomeExpiry DomainTreeRoot        `json:"outcomeExpiry"`
	Deltas        []DomainDelta         `json:"deltas,omitempty"`
}

// DomainSnapshot pins one immutable head for a bounded authenticated cursor.
// It is not a visibility root after expiry and can be collected without
// consulting provider-native timestamps.
type DomainSnapshot struct {
	SchemaVersion int                   `json:"schemaVersion"`
	DomainID      string                `json:"domainID"`
	Kind          ConsistencyDomainKind `json:"kind"`
	Head          DomainHead            `json:"head"`
	ExpiresAt     time.Time             `json:"expiresAt"`
}

// StateQuerySnapshot pins the immutable merge of every consistency domain
// selected by one StateStore prefix. It is created only when a query spans
// multiple independently mutable domains. The snapshot is cursor authority,
// never mutation authority, and becomes collectable at ExpiresAt.
type StateQuerySnapshot struct {
	SchemaVersion int            `json:"schemaVersion"`
	Prefix        string         `json:"prefix"`
	DomainID      string         `json:"domainID"`
	Root          DomainTreeRoot `json:"root"`
	ExpiresAt     time.Time      `json:"expiresAt"`
}

type DomainCatalogEntry struct {
	DomainID string                `json:"domainID"`
	Kind     ConsistencyDomainKind `json:"kind"`
	HeadKey  string                `json:"headKey"`
}

type DomainCatalogPage struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Entries       []DomainCatalogEntry `json:"entries"`
}

type DomainCatalogHead struct {
	SchemaVersion int            `json:"schemaVersion"`
	Revision      uint64         `json:"revision"`
	FreezeEpoch   uint64         `json:"freezeEpoch,omitempty"`
	Root          DomainTreeRoot `json:"root"`
}

// ProjectionHead names a rebuildable immutable view bound to one exact
// authoritative revision. It is never a mutation-authority root.
type ProjectionHead struct {
	SchemaVersion  int            `json:"schemaVersion"`
	OwnerID        string         `json:"ownerID"`
	ProjectionID   string         `json:"projectionID"`
	Kind           ProjectionKind `json:"kind"`
	SourceDomainID string         `json:"sourceDomainID"`
	SourceRevision uint64         `json:"sourceRevision"`
	SourceRoot     DomainTreeRoot `json:"sourceRoot"`
	Root           DomainTreeRoot `json:"root"`
}

func ValidateDomainHead(head DomainHead) error {
	if head.SchemaVersion != 1 || head.DomainID == "" || !validDomainKind(head.Kind) || !head.Registered {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain head")
	}
	if err := validateDomainTreeRoot(head.Base); err != nil {
		return err
	}
	if err := validateDomainTreeRoot(head.Outcomes); err != nil {
		return err
	}
	if err := validateDomainTreeRoot(head.OutcomeExpiry); err != nil {
		return err
	}
	if head.Frozen != (head.FreezeEpoch != 0) {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain freeze binding")
	}
	if head.BaseRevision > head.Revision {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain base revision")
	}
	if head.Revision == 0 && (head.BaseRevision != 0 || head.Base.Digest != "" || head.Outcomes.Digest != "" || head.OutcomeExpiry.Digest != "" || len(head.Deltas) != 0) {
		return domain.NewError(domain.ErrorInvalid, "zero-revision consistency-domain head is not empty")
	}
	previousRevision := head.BaseRevision
	for _, delta := range head.Deltas {
		if !validDomainText(delta.MutationID) || !validDomainDigest(delta.Fingerprint) || delta.Revision != previousRevision+1 || delta.RetainUntil.IsZero() || len(delta.Changes) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain delta")
		}
		previousRevision = delta.Revision
		previousKey := ""
		for _, change := range delta.Changes {
			if !validDomainText(change.Key) || previousKey != "" && change.Key <= previousKey || change.Delete && (len(change.Value) != 0 || change.LogicalVersion != "") || !change.Delete && !validDomainText(change.LogicalVersion) {
				return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain change")
			}
			previousKey = change.Key
		}
	}
	if len(head.Deltas) > 0 && head.Deltas[len(head.Deltas)-1].Revision != head.Revision {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain revision does not bind its delta window")
	}
	if len(head.Deltas) == 0 && head.BaseRevision != head.Revision {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain base does not bind its revision")
	}
	if _, err := EncodeCanonical(head); err != nil {
		return err
	}
	return nil
}

// ValidateInitialDomainHead accepts only the inert pre-registration state.
// It can expose no application values and exists solely so catalog freeze can
// never name a missing head after winning registration.
func ValidateInitialDomainHead(head DomainHead) error {
	if head.Registered || head.SchemaVersion != 1 || head.DomainID == "" || !validDomainKind(head.Kind) || head.Revision != 0 || head.BaseRevision != 0 || head.Frozen || head.FreezeEpoch != 0 || head.Base.Digest != "" || head.Outcomes.Digest != "" || head.OutcomeExpiry.Digest != "" || len(head.Deltas) != 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid initial consistency-domain head")
	}
	_, err := EncodeCanonical(head)
	return err
}

func ValidateDomainSnapshot(snapshot DomainSnapshot) error {
	if snapshot.SchemaVersion != 1 || snapshot.ExpiresAt.IsZero() || snapshot.DomainID != snapshot.Head.DomainID || snapshot.Kind != snapshot.Head.Kind {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain snapshot")
	}
	if err := ValidateDomainHead(snapshot.Head); err != nil {
		return err
	}
	_, err := EncodeCanonical(snapshot)
	return err
}

func ValidateStateQuerySnapshot(snapshot StateQuerySnapshot) error {
	if snapshot.SchemaVersion != 1 || !validDomainText(snapshot.Prefix) || !validDomainText(snapshot.DomainID) || snapshot.ExpiresAt.IsZero() {
		return domain.NewError(domain.ErrorInvalid, "invalid state-query snapshot")
	}
	if err := validateDomainTreeRoot(snapshot.Root); err != nil {
		return err
	}
	_, err := EncodeCanonical(snapshot)
	return err
}

func ValidateDomainOutcome(outcome DomainOutcome) error {
	if !validDomainText(outcome.MutationID) || !validDomainDigest(outcome.Fingerprint) || outcome.Revision == 0 || outcome.RetainUntil.IsZero() {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain outcome")
	}
	_, err := EncodeCanonical(outcome)
	return err
}

func ValidateDomainPage(page DomainPage, expectedDigest string) error {
	if page.SchemaVersion != 1 || !validDomainText(page.DomainID) || !validDomainKind(page.Kind) || page.Level < 0 || !validDomainDigest(expectedDigest) {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain page")
	}
	if page.Level == 0 {
		if len(page.Children) != 0 {
			return domain.NewError(domain.ErrorInvalid, "leaf consistency-domain page has children")
		}
		previous := ""
		for _, entry := range page.Entries {
			if !validDomainText(entry.Key) || !validDomainText(entry.LogicalVersion) || previous != "" && entry.Key <= previous {
				return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain leaf entry")
			}
			previous = entry.Key
		}
	} else {
		if len(page.Entries) != 0 || len(page.Children) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain branch page")
		}
		previous := ""
		for _, child := range page.Children {
			if !validDomainText(child.FirstKey) || child.LastKey < child.FirstKey || !validDomainText(child.LastKey) || !validDomainDigest(child.Digest) || child.Level != page.Level-1 || child.EntryCount == 0 || previous != "" && child.FirstKey <= previous {
				return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain child descriptor")
			}
			previous = child.LastKey
		}
	}
	body, err := EncodeCanonical(page)
	if err != nil {
		return err
	}
	if Digest(body) != expectedDigest {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain page digest mismatch")
	}
	return nil
}

func validateDomainTreeRoot(root DomainTreeRoot) error {
	if root.Level < 0 || root.Digest == "" && (root.EntryCount != 0 || root.ByteCount != 0 || root.Level != 0) || root.Digest != "" && !validDomainDigest(root.Digest) {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain tree root")
	}
	return nil
}

func validDomainKind(kind ConsistencyDomainKind) bool {
	switch kind {
	case DomainNamespace, DomainOwnerControl, DomainAdmin, DomainCapability, DomainShare, DomainIdentity, DomainOwnerJobs:
		return true
	default:
		return false
	}
}

func validProjectionKind(kind ProjectionKind) bool {
	switch kind {
	case ProjectionDuplicates, ProjectionAdminUsers, ProjectionAccounting, ProjectionSearch, ProjectionModified, ProjectionSize, ProjectionEntryKind:
		return true
	default:
		return false
	}
}

func ValidateDomainCatalogHead(head DomainCatalogHead) error {
	if head.SchemaVersion != 1 || head.Revision == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog head")
	}
	if err := validateDomainTreeRoot(head.Root); err != nil {
		return err
	}
	_, err := EncodeCanonical(head)
	return err
}

func ValidateDomainCatalogPage(page DomainCatalogPage, expectedDigest string) error {
	if page.SchemaVersion != 1 || len(page.Entries) == 0 || expectedDigest == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog page")
	}
	previous := ""
	for _, entry := range page.Entries {
		if !validDomainText(entry.DomainID) || !validDomainKind(entry.Kind) || entry.HeadKey != DomainHeadKey(entry.Kind, entry.DomainID).String() {
			return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog entry")
		}
		orderingKey := string(entry.Kind) + "\x00" + entry.DomainID
		if previous != "" && orderingKey <= previous {
			return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog order")
		}
		previous = orderingKey
	}
	body, err := EncodeCanonical(page)
	if err != nil {
		return err
	}
	if Digest(body) != expectedDigest {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain catalog page digest mismatch")
	}
	return nil
}

func ValidateProjectionHead(head ProjectionHead) error {
	if head.SchemaVersion != 1 || !validDomainText(head.OwnerID) || !validDomainText(head.ProjectionID) || !validProjectionKind(head.Kind) || !validDomainText(head.SourceDomainID) || head.SourceRevision == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid derived-projection head")
	}
	if err := validateDomainTreeRoot(head.SourceRoot); err != nil {
		return err
	}
	if err := validateDomainTreeRoot(head.Root); err != nil {
		return err
	}
	_, err := EncodeCanonical(head)
	return err
}

func domainKeyKind(kind ConsistencyDomainKind) string {
	if !validDomainKind(kind) {
		panic(fmt.Sprintf("invalid consistency-domain kind %q", kind))
	}
	return string(kind)
}

func projectionKeyKind(kind ProjectionKind) string {
	if !validProjectionKind(kind) {
		panic(fmt.Sprintf("invalid projection kind %q", kind))
	}
	return string(kind)
}

func validateDomainKeyPart(value string) {
	if !validDomainText(value) {
		panic("invalid empty consistency-domain key component")
	}
}

func validDomainText(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validDomainDigest(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}
