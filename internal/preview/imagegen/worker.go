package imagegen

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/applyinnovations/endlessfs/internal/preview"
)

const (
	workerEnvironment = "ENDLESSFS_INTERNAL_PREVIEW_WORKER"
	workerVersion     = 1
	maxWorkerHeader   = 4096
	maxWorkerOutput   = int64(128 << 20)
)

type workerRequest struct {
	Version    int     `json:"version"`
	SourceSize int64   `json:"sourceSize"`
	MediaType  string  `json:"mediaType"`
	Variant    int     `json:"variant"`
	Options    Options `json:"options"`
}

type workerResponse struct {
	Version int    `json:"version"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Error   string `json:"error,omitempty"`
}

// WorkerGenerator executes each untrusted codec operation in a one-shot child
// of the current EndlessFS binary. Canceling the context terminates the worker.
type WorkerGenerator struct {
	options     Options
	executable  string
	environment []string
}

func NewWorker(options Options) (*WorkerGenerator, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("preview worker executable is unavailable: %w", err)
	}
	return &WorkerGenerator{options: normalizedOptions(options), executable: executable, environment: []string{workerEnvironment + "=1"}}, nil
}

func (*WorkerGenerator) Capability() string { return "image" }
func (*WorkerGenerator) RecipeID() string   { return recipeID }
func (*WorkerGenerator) Supports(mediaType string) bool {
	return New(Options{}).Supports(mediaType)
}

func (g *WorkerGenerator) SelfTest(ctx context.Context) error {
	fixture := preview.OnePixelWebP()
	generated, err := g.Generate(ctx, preview.GenerationRequest{Source: bytes.NewReader(fixture), SourceSize: int64(len(fixture)), MediaType: "image/webp", Variant: 64})
	if err != nil || len(generated.Bytes) < 12 || generated.Width != 1 || generated.Height != 1 {
		return fmt.Errorf("preview worker integrity check failed")
	}
	return nil
}

func (g *WorkerGenerator) Generate(ctx context.Context, request preview.GenerationRequest) (preview.GeneratedArtifact, error) {
	if err := ctx.Err(); err != nil {
		return preview.GeneratedArtifact{}, err
	}
	if request.Source == nil || request.SourceSize < 1 || request.SourceSize > g.options.MaxSourceBytes || !g.Supports(request.MediaType) || request.Variant < 64 || request.Variant > 4096 {
		return preview.GeneratedArtifact{}, fmt.Errorf("image worker request is invalid")
	}
	header, err := json.Marshal(workerRequest{
		Version: workerVersion, SourceSize: request.SourceSize, MediaType: request.MediaType, Variant: request.Variant, Options: g.options,
	})
	if err != nil || len(header) > maxWorkerHeader {
		return preview.GeneratedArtifact{}, fmt.Errorf("image worker request is invalid")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(header))) // #nosec G115 -- header length is bounded to 4096 immediately above.
	// #nosec G204 -- executable is captured from os.Executable; no request value or shell is involved.
	command := exec.CommandContext(ctx, g.executable)
	command.Env = append([]string(nil), g.environment...)
	command.Stdin = io.MultiReader(bytes.NewReader(prefix[:]), bytes.NewReader(header), io.LimitReader(request.Source, request.SourceSize+1))
	output := &boundedBuffer{maximum: maxWorkerOutput + maxWorkerHeader + 4}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return preview.GeneratedArtifact{}, ctx.Err()
		}
		return preview.GeneratedArtifact{}, fmt.Errorf("image worker failed")
	}
	response, data, err := decodeWorkerResponse(output.Bytes())
	if err != nil || response.Error != "" {
		return preview.GeneratedArtifact{}, fmt.Errorf("image worker rejected source")
	}
	return preview.GeneratedArtifact{Bytes: data, Width: response.Width, Height: response.Height}, nil
}

func IsWorkerInvocation() bool { return os.Getenv(workerEnvironment) == "1" }

func RunWorker(input io.Reader, output io.Writer) error {
	header, err := readWorkerHeader(input)
	if err != nil || header.Version != workerVersion || header.SourceSize < 1 || header.SourceSize > defaultMaxSourceSize ||
		header.Options.MaxPixels < 1 || header.Options.MaxPixels > defaultMaxPixels || header.Options.MaxDimension < 1 || header.Options.MaxDimension > defaultMaxDimension ||
		header.Options.MaxSourceBytes < 1 || header.Options.MaxSourceBytes > defaultMaxSourceSize || header.SourceSize > header.Options.MaxSourceBytes {
		return fmt.Errorf("invalid preview worker request")
	}
	data, err := io.ReadAll(io.LimitReader(input, header.SourceSize+1))
	if err != nil || int64(len(data)) != header.SourceSize {
		return fmt.Errorf("invalid preview worker source")
	}
	generated, generateErr := New(header.Options).Generate(context.Background(), preview.GenerationRequest{
		Source: bytes.NewReader(data), SourceSize: header.SourceSize, MediaType: header.MediaType, Variant: header.Variant,
	})
	response := workerResponse{Version: workerVersion}
	if generateErr != nil {
		response.Error = "rejected"
		return writeWorkerResponse(output, response, nil)
	}
	response.Width, response.Height, response.Size = generated.Width, generated.Height, int64(len(generated.Bytes))
	return writeWorkerResponse(output, response, generated.Bytes)
}

func normalizedOptions(options Options) Options {
	generator := New(options)
	return Options{MaxPixels: generator.maxPixels, MaxDimension: generator.maxDimension, MaxSourceBytes: generator.maxSourceBytes}
}

func readWorkerHeader(input io.Reader) (workerRequest, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(input, prefix[:]); err != nil {
		return workerRequest{}, err
	}
	size := int(binary.BigEndian.Uint32(prefix[:]))
	if size < 2 || size > maxWorkerHeader {
		return workerRequest{}, fmt.Errorf("invalid preview worker header")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(input, data); err != nil {
		return workerRequest{}, err
	}
	var request workerRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return workerRequest{}, fmt.Errorf("invalid preview worker header")
	}
	return request, nil
}

func writeWorkerResponse(output io.Writer, response workerResponse, data []byte) error {
	header, err := json.Marshal(response)
	if err != nil || len(header) > maxWorkerHeader {
		return fmt.Errorf("invalid preview worker response")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(header))) // #nosec G115 -- header length is bounded to 4096 immediately above.
	if _, err := output.Write(prefix[:]); err != nil {
		return err
	}
	if _, err := output.Write(header); err != nil {
		return err
	}
	_, err = output.Write(data)
	return err
}

func decodeWorkerResponse(data []byte) (workerResponse, []byte, error) {
	request, err := readWorkerResponseHeader(bytes.NewReader(data))
	if err != nil || request.Version != workerVersion || request.Size < 0 || request.Size > maxWorkerOutput || request.Width < 0 || request.Height < 0 {
		return workerResponse{}, nil, fmt.Errorf("invalid preview worker response")
	}
	reader := bytes.NewReader(data)
	var prefix [4]byte
	_, _ = io.ReadFull(reader, prefix[:])
	headerSize := int(binary.BigEndian.Uint32(prefix[:]))
	if headerSize < 0 || headerSize > reader.Len() {
		return workerResponse{}, nil, fmt.Errorf("invalid preview worker response")
	}
	_, _ = reader.Seek(int64(headerSize), io.SeekCurrent)
	artifact, err := io.ReadAll(reader)
	if err != nil || int64(len(artifact)) != request.Size {
		return workerResponse{}, nil, fmt.Errorf("invalid preview worker response")
	}
	return request, artifact, nil
}

func readWorkerResponseHeader(input io.Reader) (workerResponse, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(input, prefix[:]); err != nil {
		return workerResponse{}, err
	}
	size := int(binary.BigEndian.Uint32(prefix[:]))
	if size < 2 || size > maxWorkerHeader {
		return workerResponse{}, fmt.Errorf("invalid preview worker response")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(input, data); err != nil {
		return workerResponse{}, err
	}
	var response workerResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return workerResponse{}, fmt.Errorf("invalid preview worker response")
	}
	return response, nil
}

type boundedBuffer struct {
	buffer  bytes.Buffer
	maximum int64
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if int64(b.buffer.Len())+int64(len(data)) > b.maximum {
		return 0, fmt.Errorf("preview worker output exceeded limit")
	}
	return b.buffer.Write(data)
}

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }
