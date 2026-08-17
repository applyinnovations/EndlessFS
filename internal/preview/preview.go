// Package preview owns generated-preview orchestration contracts. Original
// files remain authoritative and preview artifacts are disposable cache data.
package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/deepteams/webp"
)

const ContentTypeWebP = "image/webp"

const onePixelWebPBase64 = "UklGRkAAAABXRUJQVlA4WAoAAAAQAAAAAAAAAAAAQUxQSAIAAAAAAFZQOCAYAAAAMAEAnQEqAQABAAFAJiWkAANwAP79NmgA"

// OnePixelWebP returns the fixed, metadata-free image used by preview-store
// startup validation and deterministic contract fixtures.
func OnePixelWebP() []byte {
	data, err := base64.RawStdEncoding.DecodeString(onePixelWebPBase64)
	if err != nil {
		panic("invalid embedded preview probe")
	}
	return data
}

type Binding struct {
	Owner          domain.UserID
	ContentID      domain.ContentID
	ContentVersion domain.ContentVersion
	MediaType      string
	SourceSize     int64
	RecipeID       string
	Variant        int
}

func (b Binding) Valid() bool {
	return b.Owner.Valid() && b.ContentID != "" && b.ContentVersion != "" &&
		strings.HasPrefix(b.MediaType, "image/") && b.SourceSize >= 0 && validRecipeID(b.RecipeID) &&
		b.Variant >= 64 && b.Variant <= 4096
}

func validRecipeID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
			return false
		}
	}
	return true
}

type Artifact struct {
	GenerationID string `json:"generationID"`
	Variant      int    `json:"variant"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	ContentType  string `json:"contentType"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Bytes        []byte `json:"-"`
}

type ArtifactMetadata struct {
	GenerationID string `json:"generationID"`
	Variant      int    `json:"variant"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	ContentType  string `json:"contentType"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type GenerationClaim struct {
	ID        string
	Epoch     uint64
	ExpiresAt time.Time
}

func (c GenerationClaim) Valid() bool {
	return c.ID != "" && len(c.ID) <= 128 && !strings.ContainsAny(c.ID, "\r\n\x00/") && c.Epoch > 0 && !c.ExpiresAt.IsZero()
}

func (a Artifact) Metadata() ArtifactMetadata {
	return ArtifactMetadata{
		GenerationID: a.GenerationID, Variant: a.Variant, Width: a.Width, Height: a.Height,
		ContentType: a.ContentType, Size: a.Size, SHA256: a.SHA256,
	}
}

func (a ArtifactMetadata) ValidFor(binding Binding) bool {
	if !binding.Valid() || a.GenerationID == "" || len(a.GenerationID) > 128 || strings.ContainsAny(a.GenerationID, "\r\n\x00/") ||
		a.Variant != binding.Variant || a.Width < 1 || a.Height < 1 || a.Width > binding.Variant || a.Height > binding.Variant ||
		a.ContentType != ContentTypeWebP || a.Size < 12 {
		return false
	}
	digest, err := base64.RawURLEncoding.DecodeString(a.SHA256)
	return err == nil && len(digest) == sha256.Size
}

func (a Artifact) ValidFor(binding Binding) bool {
	if !a.Metadata().ValidFor(binding) || a.Size != int64(len(a.Bytes)) ||
		string(a.Bytes[:4]) != "RIFF" || string(a.Bytes[8:12]) != "WEBP" {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(a.SHA256)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	actual := sha256.Sum256(a.Bytes)
	if subtle.ConstantTimeCompare(actual[:], want) != 1 {
		return false
	}
	features, err := webp.GetFeatures(bytes.NewReader(a.Bytes))
	if err != nil || features.Width != a.Width || features.Height != a.Height || features.HasAnimation || features.FrameCount != 1 {
		return false
	}
	decoded, err := webp.Decode(bytes.NewReader(a.Bytes))
	return err == nil && decoded.Bounds().Dx() == a.Width && decoded.Bounds().Dy() == a.Height
}

// Store is an independently configured preview artifact provider. It never
// receives a virtual path or original-file provider key.
type Store interface {
	Validate(context.Context) error
	Check(context.Context) error
	// Claim returns the current unexpired claim together with ErrConflict so a
	// durable follower operation can await that exact immutable generation.
	Claim(context.Context, Binding, string, time.Time) (GenerationClaim, error)
	Release(context.Context, Binding, GenerationClaim) error
	Commit(context.Context, Binding, GenerationClaim, Artifact) error
	Latest(context.Context, Binding) (ArtifactMetadata, error)
	Read(context.Context, Binding, string) (Artifact, error)
	CreateDownload(context.Context, Binding, string) (domain.DownloadCapability, error)
	Ready() bool
	DataOrigin() string
}
