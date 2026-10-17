package domain

const MaxDirectoryPreparationItems = 100

func ValidateDirectoryPreparationPaths(paths []UserPath) error {
	if len(paths) < 1 || len(paths) > MaxDirectoryPreparationItems {
		return NewError(ErrorInvalid, "directory preparation must contain 1 to 100 paths")
	}
	seen := make(map[UserPath]bool, len(paths))
	for _, path := range paths {
		if !path.Valid() || path.IsRoot() || seen[path] {
			return NewError(ErrorInvalid, "invalid or duplicate directory preparation path")
		}
		seen[path] = true
	}
	return nil
}

// DirectoryPreparationResult is bound to the caller's ordered selection.
// Ensure is monotonic: an existing directory is ready, a file is never replaced.
// Missing unselected parents and file ancestors block only their descendants.
type DirectoryPreparationResult struct {
	Index     int       `json:"index"`
	Path      UserPath  `json:"path"`
	Ready     bool      `json:"ready"`
	ErrorCode ErrorKind `json:"errorCode,omitempty"`
	Retryable bool      `json:"retryable,omitempty"`
}
