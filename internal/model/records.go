// Package model defines strictly serialized application records.
package model

import (
	"encoding/base64"
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

type Session struct {
	SchemaVersion         int           `json:"schemaVersion"`
	SessionTokenHash      string        `json:"sessionTokenHash"`
	UserID                domain.UserID `json:"userID"`
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
}

func (r *Invite) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if !validOpaqueID(r.InviteID) || !validHash(r.TokenHash) || !r.CreatedByUserID.Valid() || r.MaxUses != 1 || r.Uses < 0 || r.Uses > 1 {
		return domain.NewError(domain.ErrorInvalid, "invalid invite")
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

var themeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func (r *ThemePreference) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if r.ThemeID != "system" && !themeIDPattern.MatchString(r.ThemeID) {
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
