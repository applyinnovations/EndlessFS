package theme

import (
	"embed"
	"fmt"
)

//go:embed builtin/*/theme.json builtin/*/assets/*
var builtinFiles embed.FS

func Builtins() (map[string]*ResolvedTheme, error) {
	result := make(map[string]*ResolvedTheme, 2)
	for _, directory := range []string{"light", "dark"} {
		manifestData, err := builtinFiles.ReadFile("builtin/" + directory + "/theme.json")
		if err != nil {
			return nil, err
		}
		manifest, err := DecodeManifest(manifestData)
		if err != nil {
			return nil, err
		}
		files := make(map[string][]byte)
		entries, err := builtinFiles.ReadDir("builtin/" + directory + "/assets")
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				return nil, fmt.Errorf("nested built-in assets are not expected")
			}
			content, err := builtinFiles.ReadFile("builtin/" + directory + "/assets/" + entry.Name())
			if err != nil {
				return nil, err
			}
			files["assets/"+entry.Name()] = content
		}
		resolved, err := compileBundle(Bundle{Manifest: manifest, Files: files}, nil, true)
		if err != nil {
			return nil, fmt.Errorf("built-in %s: %w", directory, err)
		}
		result[resolved.ID] = resolved
	}
	return result, nil
}
