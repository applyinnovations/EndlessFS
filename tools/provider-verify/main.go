// Command provider-verify performs read-only verification of a closed portable
// checkpoint in either a local raw-copy fixture or a configured GCS bucket.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	gcstore "github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	maximumVerificationConfigBytes = 1 << 20
	maximumFixtureBytes            = 64 << 20
)

type verificationConfig struct {
	Provider            string   `json:"provider"`
	FileBucket          string   `json:"fileBucket,omitempty"`
	StateBucket         string   `json:"stateBucket,omitempty"`
	Fixture             string   `json:"fixture,omitempty"`
	FileFixture         string   `json:"fileFixture,omitempty"`
	CheckpointID        string   `json:"checkpointID"`
	WriterSetID         string   `json:"writerSetID"`
	ConfigurationDigest string   `json:"configurationDigest"`
	KeyringIdentifiers  []string `json:"keyringIdentifiers"`
	RequiredFeatures    []string `json:"requiredFeatures,omitempty"`
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "provider verification failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 2 || args[0] != "check" || args[1] == "" {
		return domain.NewError(domain.ErrorInvalid, "usage: provider-verify check CONFIG")
	}
	configPath, err := filepath.Abs(args[1])
	if err != nil {
		return domain.WrapError(domain.ErrorInvalid, "resolve verification configuration", err)
	}
	body, err := readBoundedFile(configPath, maximumVerificationConfigBytes)
	if err != nil {
		return err
	}
	var configuration verificationConfig
	if err := state.DecodeJSONWithLimit(body, &configuration, maximumVerificationConfigBytes); err != nil {
		return err
	}
	if configuration.CheckpointID == "" {
		return domain.NewError(domain.ErrorInvalid, "checkpointID is required")
	}
	writer := portable.WriterConfiguration{
		WriterSetID: configuration.WriterSetID, ConfigurationDigest: configuration.ConfigurationDigest,
		KeyringIdentifiers: configuration.KeyringIdentifiers, RequiredFeatures: configuration.RequiredFeatures,
	}
	switch configuration.Provider {
	case "memory":
		if configuration.Fixture == "" || configuration.FileBucket != "" || configuration.StateBucket != "" {
			return domain.NewError(domain.ErrorInvalid, "memory verification requires fixture and forbids buckets")
		}
		backend, loadErr := loadMemoryFixture(configPath, configuration.Fixture)
		if loadErr != nil {
			return loadErr
		}
		var fileBackend objectstore.Backend
		if configuration.FileFixture != "" {
			fileBackend, loadErr = loadMemoryFixture(configPath, configuration.FileFixture)
			if loadErr != nil {
				return loadErr
			}
		}
		if err := portable.VerifyCheckpointReadOnlyWithFileBackend(ctx, backend, fileBackend, writer, configuration.CheckpointID); err != nil {
			return err
		}
	case "gcs":
		if configuration.FileBucket == "" || configuration.Fixture != "" || configuration.FileFixture != "" {
			return domain.NewError(domain.ErrorInvalid, "GCS verification requires fileBucket and forbids fixtures")
		}
		fileBackend, openErr := gcstore.Open(ctx, configuration.FileBucket)
		if openErr != nil {
			return openErr
		}
		defer fileBackend.Close()
		stateBucket := configuration.StateBucket
		if stateBucket == "" {
			stateBucket = configuration.FileBucket
		}
		var stateBackend = fileBackend
		var separateFileBackend objectstore.Backend
		if stateBucket != configuration.FileBucket {
			stateBackend, openErr = gcstore.Open(ctx, stateBucket)
			if openErr != nil {
				return openErr
			}
			defer stateBackend.Close()
			separateFileBackend = fileBackend
		}
		if err := portable.VerifyCheckpointReadOnlyWithFileBackend(ctx, stateBackend, separateFileBackend, writer, configuration.CheckpointID); err != nil {
			return err
		}
	default:
		return domain.NewError(domain.ErrorInvalid, "provider must be exactly memory or gcs")
	}
	_, err = fmt.Fprintf(output, "checkpoint %s verified read-only\n", configuration.CheckpointID)
	return err
}

func loadMemoryFixture(configPath, fixture string) (*objectmemory.Backend, error) {
	fixturePath := fixture
	if !filepath.IsAbs(fixturePath) {
		fixturePath = filepath.Join(filepath.Dir(configPath), fixturePath)
	}
	fixtureBody, err := readBoundedFile(fixturePath, maximumFixtureBytes)
	if err != nil {
		return nil, err
	}
	var encoded map[string]string
	if err := state.DecodeJSONWithLimit(fixtureBody, &encoded, maximumFixtureBytes); err != nil {
		return nil, err
	}
	objects := make(map[string][]byte, len(encoded))
	for key, value := range encoded {
		decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(value)
		if decodeErr != nil {
			return nil, domain.NewError(domain.ErrorInvalid, "fixture contains invalid base64 object body")
		}
		objects[key] = decoded
	}
	backend := objectmemory.New()
	if err := backend.Import(objects); err != nil {
		return nil, err
	}
	return backend, nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "resolve verification input", err)
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "open verification input directory", err)
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(absolute))
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "open verification input", err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "read verification input", err)
	}
	if len(body) == 0 || int64(len(body)) > maximum {
		return nil, domain.NewError(domain.ErrorInvalid, "verification input has invalid size")
	}
	return body, nil
}
