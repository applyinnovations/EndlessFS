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
	DependencyInventorySHA256 string   `json:"dependencyInventorySHA256"`
}

func BuildCapabilityManifest(applicationVersion string) CapabilityManifest {
	return CapabilityManifest{
		ApplicationVersion: applicationVersion, PreviewSpecification: "v1.1", Profile: "images",
		PackagedCapabilities:    []string{"image"},
		AcceptedImageMediaTypes: []string{"image/gif", "image/jpeg", "image/png", "image/webp"},
		ArtifactMediaTypes:      []string{ContentTypeWebP}, ImageRecipeID: "image-webp-q80-v1",
		DependencyInventorySHA256: DependencyInventoryDigest,
	}
}
