package theme

import (
	"archive/zip"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
	"golang.org/x/text/unicode/norm"
)

const (
	MaximumCompressedBundleBytes   int64 = 25 << 20
	MaximumUncompressedBundleBytes int64 = 50 << 20
	MaximumAssetBytes              int64 = 10 << 20
	MaximumBundleFiles                   = 512
	MaximumPathSegments                  = 8
	MaximumManifestBytes                 = 1 << 20
)

type Appearance string

const (
	AppearanceLight Appearance = "light"
	AppearanceDark  Appearance = "dark"
)

type APIVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

type FontDeclaration struct {
	Regular string `json:"regular,omitempty"`
	Bold    string `json:"bold,omitempty"`
}

type AssetReference struct {
	Path       string  `json:"path"`
	X          int     `json:"x,omitempty"`
	Y          int     `json:"y,omitempty"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	PixelRatio float64 `json:"pixelRatio,omitempty"`
	Sprite     bool    `json:"-"`
}

func (r *AssetReference) UnmarshalJSON(data []byte) error {
	if len(data) != 0 && data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		r.Path = value
		return nil
	}
	type object AssetReference
	var value object
	if err := state.DecodeJSON(data, &value); err != nil {
		return err
	}
	*r = AssetReference(value)
	r.Sprite = true
	return nil
}

func (r AssetReference) MarshalJSON() ([]byte, error) {
	if !r.Sprite {
		return json.Marshal(r.Path)
	}
	type object AssetReference
	return json.Marshal(object(r))
}

type Manifest struct {
	SchemaVersion int                        `json:"schemaVersion"`
	ThemeAPI      APIVersion                 `json:"themeAPI"`
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Version       string                     `json:"version"`
	Extends       string                     `json:"extends,omitempty"`
	Appearance    Appearance                 `json:"appearance"`
	Author        string                     `json:"author,omitempty"`
	License       string                     `json:"license"`
	Tokens        map[string]json.RawMessage `json:"tokens"`
	Fonts         map[string]FontDeclaration `json:"fonts"`
	Assets        map[string]AssetReference  `json:"assets"`
}

type Bundle struct {
	Manifest Manifest
	Files    map[string][]byte
}

var (
	themeIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)+$`)
	semverPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	spdxPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]*(?:[ ]+(?:AND|OR|WITH)[ ]+[A-Za-z0-9][A-Za-z0-9.+-]*)*$`)
)

func DecodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := state.DecodeJSONWithLimit(data, &manifest, MaximumManifestBytes); err != nil {
		return Manifest{}, domain.NewError(domain.ErrorInvalid, "invalid strict theme manifest")
	}
	return manifest, nil
}

func LoadBundle(bundlePath string) (Bundle, error) {
	info, err := os.Lstat(bundlePath)
	if err != nil {
		return Bundle{}, domain.WrapError(domain.ErrorInvalid, "theme bundle cannot be read", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme bundle cannot be a symlink")
	}
	if info.IsDir() {
		return loadDirectory(bundlePath)
	}
	if strings.ToLower(filepath.Ext(bundlePath)) != ".efstheme" {
		return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme archives must use the .efstheme extension")
	}
	if info.Size() > MaximumCompressedBundleBytes {
		return Bundle{}, domain.NewError(domain.ErrorInvalid, "compressed theme bundle exceeds 25 MiB")
	}
	return loadArchive(bundlePath, info.Size())
}

func loadDirectory(root string) (Bundle, error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return Bundle{}, domain.WrapError(domain.ErrorInvalid, "theme directory cannot be opened", err)
	}
	defer rootFS.Close()
	files := make(map[string][]byte)
	var total int64
	err = fs.WalkDir(rootFS.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !entry.IsDir() {
			return domain.NewError(domain.ErrorInvalid, "theme directories cannot contain links or devices")
		}
		if entry.IsDir() {
			return nil
		}
		if metadata, ok := info.Sys().(*syscall.Stat_t); ok && metadata.Nlink > 1 {
			return domain.NewError(domain.ErrorInvalid, "theme directories cannot contain hard-linked files")
		}
		normalized, err := normalizeBundlePath(filepath.ToSlash(name))
		if err != nil {
			return err
		}
		if len(files) >= MaximumBundleFiles {
			return domain.NewError(domain.ErrorInvalid, "theme bundle exceeds 512 files")
		}
		if info.Size() > MaximumAssetBytes && normalized != "theme.json" {
			return domain.NewError(domain.ErrorInvalid, "theme asset exceeds 10 MiB")
		}
		total += info.Size()
		if total > MaximumUncompressedBundleBytes {
			return domain.NewError(domain.ErrorInvalid, "theme bundle exceeds 50 MiB uncompressed")
		}
		content, err := fs.ReadFile(rootFS.FS(), name)
		if err != nil {
			return err
		}
		if _, duplicate := files[normalized]; duplicate {
			return domain.NewError(domain.ErrorInvalid, "theme bundle has duplicate normalized paths")
		}
		files[normalized] = content
		return nil
	})
	if err != nil {
		return Bundle{}, domain.WrapError(domain.ErrorInvalid, "invalid theme directory", err)
	}
	return bundleFromFiles(files)
}

func loadArchive(name string, compressedSize int64) (Bundle, error) {
	archive, err := zip.OpenReader(name)
	if err != nil {
		return Bundle{}, domain.NewError(domain.ErrorInvalid, "invalid theme ZIP archive")
	}
	defer archive.Close()
	if len(archive.File) > MaximumBundleFiles {
		return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme bundle exceeds 512 files")
	}
	files := make(map[string][]byte)
	var total int64
	for _, item := range archive.File {
		if item.FileInfo().IsDir() {
			continue
		}
		if item.Mode()&os.ModeSymlink != 0 || !item.Mode().IsRegular() {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme archive cannot contain links or devices")
		}
		normalized, err := normalizeBundlePath(item.Name)
		if err != nil {
			return Bundle{}, err
		}
		if _, duplicate := files[normalized]; duplicate {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme archive has duplicate normalized paths")
		}
		if item.UncompressedSize64 > uint64(MaximumUncompressedBundleBytes)+1 || item.CompressedSize64 > uint64(MaximumCompressedBundleBytes)+1 {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme archive entry declares an excessive size")
		}
		uncompressed := int64(item.UncompressedSize64) // #nosec G115 -- bounded immediately above.
		compressed := int64(item.CompressedSize64)     // #nosec G115 -- bounded immediately above.
		if uncompressed > MaximumAssetBytes && normalized != "theme.json" {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme asset exceeds 10 MiB")
		}
		total += uncompressed
		if total > MaximumUncompressedBundleBytes {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme bundle exceeds 50 MiB uncompressed")
		}
		if uncompressed > 1<<20 && (compressed == 0 || uncompressed > compressed*100) {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme archive compression ratio is unsafe")
		}
		reader, err := item.Open()
		if err != nil {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme archive entry cannot be opened")
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, min(uncompressed+1, MaximumUncompressedBundleBytes+1)))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || int64(len(content)) != uncompressed {
			return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme archive entry size is invalid")
		}
		files[normalized] = content
	}
	if compressedSize > MaximumCompressedBundleBytes {
		return Bundle{}, domain.NewError(domain.ErrorInvalid, "compressed theme bundle exceeds 25 MiB")
	}
	return bundleFromFiles(files)
}

func bundleFromFiles(files map[string][]byte) (Bundle, error) {
	manifestBytes, found := files["theme.json"]
	if !found {
		return Bundle{}, domain.NewError(domain.ErrorInvalid, "theme.json is required")
	}
	manifest, err := DecodeManifest(manifestBytes)
	if err != nil {
		return Bundle{}, err
	}
	delete(files, "theme.json")
	return Bundle{Manifest: manifest, Files: files}, nil
}

func normalizeBundlePath(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) {
		return "", domain.NewError(domain.ErrorInvalid, "invalid theme bundle path")
	}
	value = norm.NFC.String(value)
	if path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return "", domain.NewError(domain.ErrorInvalid, "theme path traversal is forbidden")
	}
	segments := strings.Split(value, "/")
	if len(segments) > MaximumPathSegments {
		return "", domain.NewError(domain.ErrorInvalid, "theme path nesting exceeds eight segments")
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", domain.NewError(domain.ErrorInvalid, "invalid theme path segment")
		}
	}
	if value != "theme.json" && !strings.HasPrefix(value, "assets/") {
		return "", domain.NewError(domain.ErrorInvalid, "theme files must be theme.json or beneath assets")
	}
	forbidden := map[string]bool{".css": true, ".scss": true, ".sass": true, ".less": true, ".html": true, ".htm": true, ".js": true, ".mjs": true, ".cjs": true, ".wasm": true, ".md": true, ".markdown": true, ".tmpl": true, ".tpl": true}
	if forbidden[strings.ToLower(path.Ext(value))] {
		return "", domain.NewError(domain.ErrorInvalid, "theme bundles cannot contain code or markup files")
	}
	return value, nil
}

func validateManifestMetadata(manifest Manifest, builtIn bool) error {
	if manifest.SchemaVersion != 1 || manifest.ThemeAPI.Major != APIMajor || manifest.ThemeAPI.Minor < 0 || manifest.ThemeAPI.Minor > APIMinor {
		return domain.NewError(domain.ErrorInvalid, "incompatible theme schema or Theme API")
	}
	if len(manifest.ID) > 128 || !themeIDPattern.MatchString(manifest.ID) {
		return domain.NewError(domain.ErrorInvalid, "invalid reverse-domain theme ID")
	}
	if strings.HasPrefix(manifest.ID, "endlessfs-") != builtIn {
		return domain.NewError(domain.ErrorInvalid, "reserved built-in theme ID")
	}
	if !validText(manifest.Name, 1, 128) || !validTextOptional(manifest.Author, 128) || !semverPattern.MatchString(manifest.Version) || !validLicense(manifest.License) {
		return domain.NewError(domain.ErrorInvalid, "invalid theme metadata")
	}
	if manifest.Appearance != AppearanceLight && manifest.Appearance != AppearanceDark {
		return domain.NewError(domain.ErrorInvalid, "invalid theme appearance")
	}
	if builtIn {
		if manifest.Extends != "" || (manifest.ID != "endlessfs-light" && manifest.ID != "endlessfs-dark") {
			return domain.NewError(domain.ErrorInvalid, "invalid built-in theme")
		}
	} else if manifest.Extends != "endlessfs-light" && manifest.Extends != "endlessfs-dark" {
		return domain.NewError(domain.ErrorInvalid, "custom themes must directly extend one built-in")
	}
	return nil
}

func validText(value string, minimum, maximum int) bool {
	return utf8.ValidString(value) && len(value) >= minimum && len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}
func validTextOptional(value string, maximum int) bool {
	return value == "" || validText(value, 1, maximum)
}
func validLicense(value string) bool {
	if strings.HasPrefix(value, "LicenseRef-") {
		return validText(value, 12, 128) && !strings.Contains(value, "/")
	}
	return len(value) <= 256 && spdxPattern.MatchString(value)
}

func encodeManifest(manifest Manifest) []byte { value, _ := json.Marshal(manifest); return value }
