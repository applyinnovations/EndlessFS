// Package model defines strictly serialized application records.
package model

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const SchemaVersion = 1

type Profile struct {
	UserID      domain.UserID      `json:"userID"`
	DisplayName domain.DisplayName `json:"displayName"`
}

func (r *Profile) Validate() error {
	if !r.UserID.Valid() || r.DisplayName.String() == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid profile")
	}
	return nil
}

type AccountStatus string

const (
	AccountEnabled  AccountStatus = "enabled"
	AccountDisabled AccountStatus = "disabled"
)

type Account struct {
	SchemaVersion int           `json:"schemaVersion"`
	UserID        domain.UserID `json:"userID"`
	Status        AccountStatus `json:"status"`
	AuthEpoch     uint64        `json:"authEpoch,omitempty"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
}

func (r *Account) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !r.UserID.Valid() || (r.Status != AccountEnabled && r.Status != AccountDisabled) {
		return domain.NewError(domain.ErrorInvalid, "invalid account")
	}
	return validateTimes(r.CreatedAt, r.UpdatedAt)
}

type Credential struct {
	SchemaVersion  int                     `json:"schemaVersion"`
	CredentialID   string                  `json:"credentialID"`
	UserID         domain.UserID           `json:"userID"`
	PublicKey      string                  `json:"publicKey"`
	SignCount      uint32                  `json:"signCount"`
	Transports     []string                `json:"transports,omitempty"`
	BackupEligible *bool                   `json:"backupEligible,omitempty"`
	BackupState    *bool                   `json:"backupState,omitempty"`
	Label          *domain.CredentialLabel `json:"label,omitempty"`
	CreatedAt      time.Time               `json:"createdAt"`
	LastUsedAt     time.Time               `json:"lastUsedAt"`
}

func (r *Credential) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !r.UserID.Valid() || !validBase64URL(r.CredentialID, 1) || !validBase64URL(r.PublicKey, 1) {
		return domain.NewError(domain.ErrorInvalid, "invalid credential")
	}
	if r.Label != nil && r.Label.String() == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid credential label")
	}
	return validateTimes(r.CreatedAt, r.LastUsedAt)
}

type CredentialIndex struct {
	SchemaVersion int           `json:"schemaVersion"`
	UserID        domain.UserID `json:"userID"`
	CredentialIDs []string      `json:"credentialIDs"`
}

func (r *CredentialIndex) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !r.UserID.Valid() || len(r.CredentialIDs) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid credential index")
	}
	seen := make(map[string]struct{}, len(r.CredentialIDs))
	for _, credentialID := range r.CredentialIDs {
		if !validBase64URL(credentialID, 1) {
			return domain.NewError(domain.ErrorInvalid, "invalid indexed credential ID")
		}
		if _, exists := seen[credentialID]; exists {
			return domain.NewError(domain.ErrorInvalid, "duplicate indexed credential ID")
		}
		seen[credentialID] = struct{}{}
	}
	sort.Strings(r.CredentialIDs)
	return nil
}

type CeremonyType string

const (
	CeremonyRegistration   CeremonyType = "registration"
	CeremonyAuthentication CeremonyType = "authentication"
)

type CeremonyFlow string

const (
	CeremonyBootstrap          CeremonyFlow = "bootstrap"
	CeremonyPublic             CeremonyFlow = "public"
	CeremonyInvite             CeremonyFlow = "invite"
	CeremonyAuthenticationFlow CeremonyFlow = "authentication"
	CeremonyAddPasskey         CeremonyFlow = "add_passkey"
	CeremonyRecovery           CeremonyFlow = "recovery"
)

// Ceremony stores opaque library session data server-side. BrowserBindingHash
// binds it to the browser without storing the raw binding cookie.
type Ceremony struct {
	SchemaVersion      int                     `json:"schemaVersion"`
	CeremonyID         string                  `json:"ceremonyID"`
	Type               CeremonyType            `json:"type"`
	Flow               CeremonyFlow            `json:"flow"`
	BrowserBindingHash string                  `json:"browserBindingHash"`
	UserID             *domain.UserID          `json:"userID,omitempty"`
	DisplayName        *domain.DisplayName     `json:"displayName,omitempty"`
	CredentialLabel    *domain.CredentialLabel `json:"credentialLabel,omitempty"`
	BearerTokenHash    string                  `json:"bearerTokenHash,omitempty"`
	LibrarySession     json.RawMessage         `json:"librarySession"`
	CreatedAt          time.Time               `json:"createdAt"`
	ExpiresAt          time.Time               `json:"expiresAt"`
	ConsumedAt         *time.Time              `json:"consumedAt,omitempty"`
	OperationID        string                  `json:"operationID,omitempty"`
}

func (r *Ceremony) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !validOpaqueID(r.CeremonyID) || !validHash(r.BrowserBindingHash) || len(r.LibrarySession) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony")
	}
	if r.Type != CeremonyRegistration && r.Type != CeremonyAuthentication {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony type")
	}
	if !validCeremonyFlow(r.Type, r.Flow) {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony flow")
	}
	if r.UserID != nil && !r.UserID.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony user")
	}
	if r.DisplayName != nil && r.DisplayName.String() == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony display name")
	}
	if r.CredentialLabel != nil && r.CredentialLabel.String() == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony credential label")
	}
	if r.BearerTokenHash != "" && !validHash(r.BearerTokenHash) {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony bearer hash")
	}
	if err := validateTimes(r.CreatedAt, r.ExpiresAt); err != nil || !r.ExpiresAt.After(r.CreatedAt) {
		return domain.NewError(domain.ErrorInvalid, "invalid ceremony times")
	}
	if err := validateOptionalTimes(r.ConsumedAt); err != nil {
		return err
	}
	if r.ConsumedAt == nil && r.OperationID != "" {
		return domain.NewError(domain.ErrorInvalid, "unused ceremony has an operation")
	}
	return nil
}

func validCeremonyFlow(ceremonyType CeremonyType, flow CeremonyFlow) bool {
	if ceremonyType == CeremonyAuthentication {
		return flow == CeremonyAuthenticationFlow
	}
	switch flow {
	case CeremonyBootstrap, CeremonyPublic, CeremonyInvite, CeremonyAddPasskey, CeremonyRecovery:
		return true
	default:
		return false
	}
}

type OperationStatus string

const (
	OperationClaimed   OperationStatus = "claimed"
	OperationCommitted OperationStatus = "committed"
)

// RegistrationOperation is a durable state machine for multi-record identity
// changes. Credential contains only verified public material.
type RegistrationOperation struct {
	SchemaVersion int                `json:"schemaVersion"`
	OperationID   string             `json:"operationID"`
	Flow          CeremonyFlow       `json:"flow"`
	Status        OperationStatus    `json:"status"`
	UserID        domain.UserID      `json:"userID"`
	DisplayName   domain.DisplayName `json:"displayName"`
	Credential    Credential         `json:"credential"`
	CreatedAt     time.Time          `json:"createdAt"`
	CommittedAt   *time.Time         `json:"committedAt,omitempty"`
}

func (r *RegistrationOperation) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !validOpaqueID(r.OperationID) || !r.UserID.Valid() || r.DisplayName.String() == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid registration operation")
	}
	if !validCeremonyFlow(CeremonyRegistration, r.Flow) {
		return domain.NewError(domain.ErrorInvalid, "invalid registration flow")
	}
	if r.Status != OperationClaimed && r.Status != OperationCommitted {
		return domain.NewError(domain.ErrorInvalid, "invalid registration operation status")
	}
	if err := r.Credential.Validate(); err != nil {
		return err
	}
	if err := validateUTC(r.CreatedAt); err != nil {
		return err
	}
	if err := validateOptionalTimes(r.CommittedAt); err != nil {
		return err
	}
	if (r.Status == OperationCommitted) != (r.CommittedAt != nil) {
		return domain.NewError(domain.ErrorInvalid, "registration commit state mismatch")
	}
	return nil
}

type BootstrapState struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Status        OperationStatus       `json:"status"`
	Operation     RegistrationOperation `json:"operation"`
	CompletedAt   *time.Time            `json:"completedAt,omitempty"`
}

func (r *BootstrapState) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if r.Status != OperationClaimed && r.Status != OperationCommitted {
		return domain.NewError(domain.ErrorInvalid, "invalid bootstrap status")
	}
	if err := r.Operation.Validate(); err != nil {
		return err
	}
	if r.Operation.Flow != CeremonyBootstrap || r.Operation.Status != r.Status {
		return domain.NewError(domain.ErrorInvalid, "bootstrap operation mismatch")
	}
	if err := validateOptionalTimes(r.CompletedAt); err != nil {
		return err
	}
	if (r.Status == OperationCommitted) != (r.CompletedAt != nil) {
		return domain.NewError(domain.ErrorInvalid, "bootstrap completion mismatch")
	}
	return nil
}

type FirstAccountMarker struct {
	SchemaVersion int           `json:"schemaVersion"`
	Flow          CeremonyFlow  `json:"flow"`
	OperationID   string        `json:"operationID"`
	UserID        domain.UserID `json:"userID"`
	CreatedAt     time.Time     `json:"createdAt"`
}

func (r *FirstAccountMarker) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if r.Flow != CeremonyBootstrap && r.Flow != CeremonyPublic && r.Flow != CeremonyInvite {
		return domain.NewError(domain.ErrorInvalid, "invalid first-account flow")
	}
	if !validOpaqueID(r.OperationID) || !r.UserID.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid first-account marker")
	}
	return validateUTC(r.CreatedAt)
}

type IdempotencyKind string

const (
	IdempotencyInvite   IdempotencyKind = "invite"
	IdempotencyRecovery IdempotencyKind = "recovery"
)

type IdempotencyRecord struct {
	SchemaVersion int             `json:"schemaVersion"`
	OwnerUserID   domain.UserID   `json:"ownerUserID"`
	KeyHash       string          `json:"keyHash"`
	Kind          IdempotencyKind `json:"kind"`
	Fingerprint   string          `json:"fingerprint"`
	ResourceID    string          `json:"resourceID"`
	Resource      json.RawMessage `json:"resource"`
	Status        OperationStatus `json:"status"`
	CreatedAt     time.Time       `json:"createdAt"`
	CommittedAt   *time.Time      `json:"committedAt,omitempty"`
}

func (r *IdempotencyRecord) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !r.OwnerUserID.Valid() || !validHash(r.KeyHash) || !validHash(r.Fingerprint) || !validOpaqueID(r.ResourceID) || len(r.Resource) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid idempotency record")
	}
	if r.Kind != IdempotencyInvite && r.Kind != IdempotencyRecovery {
		return domain.NewError(domain.ErrorInvalid, "invalid idempotency kind")
	}
	if r.Status != OperationClaimed && r.Status != OperationCommitted {
		return domain.NewError(domain.ErrorInvalid, "invalid idempotency status")
	}
	if err := validateUTC(r.CreatedAt); err != nil {
		return err
	}
	if err := validateOptionalTimes(r.CommittedAt); err != nil {
		return err
	}
	if (r.Status == OperationCommitted) != (r.CommittedAt != nil) {
		return domain.NewError(domain.ErrorInvalid, "idempotency commit state mismatch")
	}
	return nil
}

type Session struct {
	SchemaVersion         int           `json:"schemaVersion"`
	SessionTokenHash      string        `json:"sessionTokenHash"`
	UserID                domain.UserID `json:"userID"`
	AuthEpoch             uint64        `json:"authEpoch,omitempty"`
	CSRFTokenHash         string        `json:"csrfTokenHash"`
	CreatedAt             time.Time     `json:"createdAt"`
	ExpiresAt             time.Time     `json:"expiresAt"`
	LastSeenAt            *time.Time    `json:"lastSeenAt,omitempty"`
	AuthnCredentialIDHash string        `json:"authnCredentialIDHash"`
}

func (r *Session) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !r.UserID.Valid() || !validHash(r.SessionTokenHash) || !validHash(r.CSRFTokenHash) || !validHash(r.AuthnCredentialIDHash) {
		return domain.NewError(domain.ErrorInvalid, "invalid session")
	}
	if err := validateTimes(r.CreatedAt, r.ExpiresAt); err != nil || !r.ExpiresAt.After(r.CreatedAt) {
		return domain.NewError(domain.ErrorInvalid, "invalid session times")
	}
	if r.LastSeenAt != nil && validateUTC(*r.LastSeenAt) != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid session last-seen time")
	}
	return nil
}

type Invite struct {
	SchemaVersion   int            `json:"schemaVersion"`
	InviteID        string         `json:"inviteID"`
	TokenHash       string         `json:"tokenHash"`
	CreatedByUserID domain.UserID  `json:"createdByUserID"`
	CreatedAt       time.Time      `json:"createdAt"`
	ExpiresAt       *time.Time     `json:"expiresAt,omitempty"`
	MaxUses         int            `json:"maxUses"`
	Uses            int            `json:"uses"`
	UsedAt          *time.Time     `json:"usedAt,omitempty"`
	UsedByUserID    *domain.UserID `json:"usedByUserID,omitempty"`
	RevokedAt       *time.Time     `json:"revokedAt,omitempty"`
	OperationID     string         `json:"operationID,omitempty"`
}

func (r *Invite) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !validOpaqueID(r.InviteID) || !validHash(r.TokenHash) || !r.CreatedByUserID.Valid() || r.MaxUses != 1 || r.Uses < 0 || r.Uses > 1 {
		return domain.NewError(domain.ErrorInvalid, "invalid invite")
	}
	if r.Uses == 0 && (r.UsedAt != nil || r.UsedByUserID != nil || r.OperationID != "") {
		return domain.NewError(domain.ErrorInvalid, "unused invite has consumption state")
	}
	if r.Uses == 1 && (r.UsedAt == nil || r.UsedByUserID == nil || !r.UsedByUserID.Valid() || !validOpaqueID(r.OperationID)) {
		return domain.NewError(domain.ErrorInvalid, "used invite lacks consumption state")
	}
	if err := validateUTC(r.CreatedAt); err != nil {
		return err
	}
	return validateOptionalTimes(r.ExpiresAt, r.UsedAt, r.RevokedAt)
}

type Recovery struct {
	SchemaVersion   int           `json:"schemaVersion"`
	RecoveryID      string        `json:"recoveryID"`
	TokenHash       string        `json:"tokenHash"`
	TargetUserID    domain.UserID `json:"targetUserID"`
	CreatedByUserID domain.UserID `json:"createdByUserID"`
	CreatedAt       time.Time     `json:"createdAt"`
	ExpiresAt       time.Time     `json:"expiresAt"`
	UsedAt          *time.Time    `json:"usedAt,omitempty"`
	RevokedAt       *time.Time    `json:"revokedAt,omitempty"`
	OperationID     string        `json:"operationID,omitempty"`
}

func (r *Recovery) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !validOpaqueID(r.RecoveryID) || !validHash(r.TokenHash) || !r.TargetUserID.Valid() || !r.CreatedByUserID.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid recovery")
	}
	if err := validateTimes(r.CreatedAt, r.ExpiresAt); err != nil || !r.ExpiresAt.After(r.CreatedAt) {
		return domain.NewError(domain.ErrorInvalid, "invalid recovery times")
	}
	if r.UsedAt == nil && r.OperationID != "" {
		return domain.NewError(domain.ErrorInvalid, "unused recovery has an operation")
	}
	if r.UsedAt != nil && !validOpaqueID(r.OperationID) {
		return domain.NewError(domain.ErrorInvalid, "used recovery lacks an operation")
	}
	return validateOptionalTimes(r.UsedAt, r.RevokedAt)
}

type Share struct {
	SchemaVersion int              `json:"schemaVersion"`
	ShareID       string           `json:"shareID"`
	TokenHash     string           `json:"tokenHash"`
	OwnerUserID   domain.UserID    `json:"ownerUserID"`
	RootPath      domain.UserPath  `json:"rootPath"`
	RootVersion   domain.Version   `json:"rootVersion"`
	Kind          domain.EntryKind `json:"kind"`
	CreatedAt     time.Time        `json:"createdAt"`
	ExpiresAt     *time.Time       `json:"expiresAt,omitempty"`
	RevokedAt     *time.Time       `json:"revokedAt,omitempty"`
}

func (r *Share) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !validOpaqueID(r.ShareID) || !validHash(r.TokenHash) || !r.OwnerUserID.Valid() || !r.RootPath.Valid() || r.RootVersion == "" || (r.Kind != domain.EntryFile && r.Kind != domain.EntryDirectory) {
		return domain.NewError(domain.ErrorInvalid, "invalid share")
	}
	if err := validateUTC(r.CreatedAt); err != nil {
		return err
	}
	return validateOptionalTimes(r.ExpiresAt, r.RevokedAt)
}

type Trash struct {
	SchemaVersion   int              `json:"schemaVersion"`
	TrashID         string           `json:"trashID"`
	OwnerUserID     domain.UserID    `json:"ownerUserID"`
	OriginalPath    domain.UserPath  `json:"originalPath"`
	TrashedPath     domain.UserPath  `json:"trashedPath"`
	Kind            domain.EntryKind `json:"kind"`
	TrashedAt       time.Time        `json:"trashedAt"`
	OriginalVersion domain.Version   `json:"originalVersion"`
}

// BatchOperation persists the aggregate state returned for bounded multi-item
// control requests. Provider operation details remain provider-neutral.
type BatchOperation struct {
	SchemaVersion    int                   `json:"schemaVersion"`
	OwnerUserID      domain.UserID         `json:"ownerUserID"`
	OperationID      domain.OperationID    `json:"operationID"`
	Kind             string                `json:"kind,omitempty"`
	RequestDigest    string                `json:"requestDigest,omitempty"`
	ItemCount        int                   `json:"itemCount,omitempty"`
	SucceededCount   int                   `json:"succeededCount,omitempty"`
	ReclaimableBytes int64                 `json:"reclaimableBytes,omitempty"`
	State            domain.OperationState `json:"state"`
	StartedAt        time.Time             `json:"startedAt"`
	UpdatedAt        time.Time             `json:"updatedAt"`
}

type MutationOutcome struct {
	SchemaVersion int              `json:"schemaVersion"`
	OwnerUserID   domain.UserID    `json:"ownerUserID"`
	KeyHash       string           `json:"keyHash"`
	Kind          string           `json:"kind"`
	Fingerprint   string           `json:"fingerprint"`
	Operation     domain.Operation `json:"operation"`
}

func (r *MutationOutcome) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !r.OwnerUserID.Valid() || !validHash(r.KeyHash) || !validHash(r.Fingerprint) || (r.Kind != "restore" && r.Kind != "permanent_delete") || r.Operation.ID == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid mutation outcome")
	}
	if r.Operation.State != domain.OperationSucceeded && r.Operation.State != domain.OperationFailed {
		return domain.NewError(domain.ErrorInvalid, "mutation outcome is incomplete")
	}
	return validateTimes(r.Operation.StartedAt, r.Operation.UpdatedAt)
}

func (r *BatchOperation) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !r.OwnerUserID.Valid() || r.OperationID == "" || (r.State != domain.OperationSucceeded && r.State != domain.OperationFailed && r.State != domain.OperationRunning && r.State != domain.OperationPending) || r.ItemCount < 0 || r.SucceededCount < 0 || r.SucceededCount > r.ItemCount || r.ReclaimableBytes < 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid batch operation")
	}
	if r.Kind == "" {
		if r.RequestDigest != "" || r.ItemCount != 0 || r.SucceededCount != 0 || r.ReclaimableBytes != 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid legacy batch operation metadata")
		}
	} else if r.Kind != "duplicate_reconciliation" || !validHash(r.RequestDigest) || r.ItemCount == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid batch operation audit metadata")
	}
	return validateTimes(r.StartedAt, r.UpdatedAt)
}

func (r *Trash) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !validOpaqueID(r.TrashID) || !r.OwnerUserID.Valid() || !r.OriginalPath.Valid() || !r.TrashedPath.Valid() || r.OriginalVersion == "" || (r.Kind != domain.EntryFile && r.Kind != domain.EntryDirectory) {
		return domain.NewError(domain.ErrorInvalid, "invalid trash record")
	}
	return validateUTC(r.TrashedAt)
}

type ThemePreference struct {
	SchemaVersion int    `json:"schemaVersion"`
	ThemeID       string `json:"themeID"`
}

var themeIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)+$`)

func (r *ThemePreference) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if r.ThemeID != "system" && (len(r.ThemeID) > 128 || !themeIDPattern.MatchString(r.ThemeID)) {
		return domain.NewError(domain.ErrorInvalid, "invalid theme preference")
	}
	return nil
}

type AdminRoles struct {
	SchemaVersion int             `json:"schemaVersion"`
	UserIDs       []domain.UserID `json:"userIDs"`
}

func (r *AdminRoles) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if len(r.UserIDs) == 0 {
		return domain.NewError(domain.ErrorInvalid, "at least one administrator is required")
	}
	seen := make(map[string]struct{}, len(r.UserIDs))
	for _, userID := range r.UserIDs {
		if !userID.Valid() {
			return domain.NewError(domain.ErrorInvalid, "invalid administrator user ID")
		}
		if _, exists := seen[userID.String()]; exists {
			return domain.NewError(domain.ErrorInvalid, "duplicate administrator user ID")
		}
		seen[userID.String()] = struct{}{}
	}
	sort.Slice(r.UserIDs, func(left, right int) bool { return r.UserIDs[left].String() < r.UserIDs[right].String() })
	return nil
}

func validateSchema(version int) error {
	if version != SchemaVersion {
		return domain.NewError(domain.ErrorInvalid, "unsupported record schema version")
	}
	return nil
}

func validateTimes(values ...time.Time) error {
	for _, value := range values {
		if err := validateUTC(value); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalTimes(values ...*time.Time) error {
	for _, value := range values {
		if value != nil {
			if err := validateUTC(*value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateUTC(value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC {
		return domain.NewError(domain.ErrorInvalid, "record time must be non-zero UTC")
	}
	return nil
}

func validOpaqueID(value string) bool {
	return validBase64URL(value, 16)
}

func validHash(value string) bool {
	return validBase64URL(value, 32)
}

func validBase64URL(value string, minimumBytes int) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) >= minimumBytes && base64.RawURLEncoding.EncodeToString(decoded) == value
}
