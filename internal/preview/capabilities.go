package preview

// DependencyInventoryDigest is injected by the locked Nix build. Development
// builds retain a truthful marker instead of pretending to be a release build.
var DependencyInventoryDigest = "development"

type CapabilityManifest struct {
	ApplicationVersion        string   `json:"applicationVersion"`
	PreviewSpecification      string   `json:"previewSpecification"`
	Profile                   string   `json:"profile"`
	PackagedCapabilities      []string `json:"packagedCapabilities"`
	AcceptedImageMediaTypes   []string `json:"acceptedImageMediaTypes"`
	ArtifactMediaTypes        []string `json:"artifactMediaTypes"`
	ImageRecipeID             string   `json:"imageRecipeID"`
	PackagedImageDecoders     []string `json:"packagedImageDecoders"`
	DependencyInventorySHA256 string   `json:"dependencyInventorySHA256"`
}

func BuildCapabilityManifest(applicationVersion string) CapabilityManifest {
	return CapabilityManifest{
		ApplicationVersion: applicationVersion, PreviewSpecification: "v1.1", Profile: "images",
		PackagedCapabilities: []string{"image"},
		AcceptedImageMediaTypes: []string{
			"image/gif", "image/jpeg", "image/png", "image/webp", "image/x-adobe-dng", "image/x-canon-cr2", "image/x-canon-cr3",
			"image/x-fuji-raf", "image/x-nikon-nef", "image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-pentax-pef", "image/x-sony-arw",
		},
		ArtifactMediaTypes: []string{ContentTypeWebP}, ImageRecipeID: "image-webp-q80-v1",
		PackagedImageDecoders:     []string{"go-standard-library", "deepteams-webp-1.2.6", "libraw-0.22.1"},
		DependencyInventorySHA256: DependencyInventoryDigest,
	}
}
