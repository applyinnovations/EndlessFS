package imagegen

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/applyinnovations/endlessfs/internal/preview"
)

const maxRawDecoderOutput = int64(128 << 20)

func isRawMediaType(mediaType string) bool {
	switch mediaType {
	case "image/x-adobe-dng", "image/x-canon-cr2", "image/x-canon-cr3", "image/x-fuji-raf", "image/x-nikon-nef",
		"image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-pentax-pef", "image/x-sony-arw":
		return true
	default:
		return false
	}
}

func validateRawDecoder(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("RAW decoder path is not absolute")
	}
	// #nosec G703 -- this path is supplied only by the Nix package or the
	// package's internal test wrapper, never by an HTTP, provider, or user input.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("RAW decoder is not executable")
	}
	return nil
}

// PackagedRawDecoderPath resolves only the fixed decoder installed beside the
// EndlessFS binary. The internal environment fallback is set by Nix's test and
// development wrappers, never by an application request or configuration.
func PackagedRawDecoderPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("RAW decoder is unavailable")
	}
	candidate := filepath.Join(filepath.Dir(executable), "endlessfs-raw-decoder")
	if validateRawDecoder(candidate) == nil {
		return candidate, nil
	}
	if candidate = os.Getenv(rawDecoderEnvironment); candidate != "" && validateRawDecoder(candidate) == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("RAW decoder is unavailable")
}

func (g *Generator) generateRAW(ctx context.Context, request preview.GenerationRequest, data []byte) (preview.GeneratedArtifact, error) {
	if err := validateRawDecoder(g.rawDecoderPath); err != nil {
		return preview.GeneratedArtifact{}, fmt.Errorf("RAW decoder is unavailable")
	}
	input, inputPath, err := anonymousRawInput(data)
	if err != nil {
		return preview.GeneratedArtifact{}, fmt.Errorf("RAW input staging failed")
	}
	defer input.Close()
	// #nosec G204 -- the decoder path is an absolute, executable Nix input and
	// every argument is fixed by this closed generator implementation.
	command := exec.CommandContext(ctx, g.rawDecoderPath, "-h", "-o", "1", "-Z", "-", inputPath)
	command.Env = []string{}
	command.ExtraFiles = []*os.File{input}
	protectRawDecoderChild(command)
	output := &boundedBuffer{maximum: maxRawDecoderOutput}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return preview.GeneratedArtifact{}, ctx.Err()
		}
		return preview.GeneratedArtifact{}, fmt.Errorf("RAW decode failed")
	}
	decoded, err := decodePPM(output.Bytes(), g.maxDimension, g.maxPixels)
	if err != nil {
		return preview.GeneratedArtifact{}, fmt.Errorf("RAW decoder output is invalid")
	}
	if err := ctx.Err(); err != nil {
		return preview.GeneratedArtifact{}, err
	}
	resized := resizeToMaximumEdge(decoded, request.Variant)
	encoded, err := encodeWebP(resized)
	if err != nil {
		return preview.GeneratedArtifact{}, err
	}
	bounds := resized.Bounds()
	return preview.GeneratedArtifact{Bytes: encoded, Width: bounds.Dx(), Height: bounds.Dy()}, nil
}

func decodePPM(data []byte, maximumDimension, maximumPixels int) (image.Image, error) {
	reader := bufio.NewReader(bytes.NewReader(data))
	magic, err := readPPMToken(reader)
	if err != nil || magic != "P6" {
		return nil, fmt.Errorf("invalid PPM magic")
	}
	width, err := readPPMInt(reader)
	if err != nil {
		return nil, err
	}
	height, err := readPPMInt(reader)
	if err != nil {
		return nil, err
	}
	maximum, err := readPPMInt(reader)
	if err != nil || maximum != 255 || width < 1 || height < 1 || width > maximumDimension || height > maximumDimension || width > maximumPixels/height {
		return nil, fmt.Errorf("PPM dimensions exceed generator limits")
	}
	pixels := make([]byte, width*height*3)
	if _, err := io.ReadFull(reader, pixels); err != nil {
		return nil, fmt.Errorf("invalid PPM pixels")
	}
	if trailing, err := io.ReadAll(reader); err != nil || len(trailing) != 0 {
		return nil, fmt.Errorf("invalid PPM trailing bytes")
	}
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for index := range width * height {
		result.SetNRGBA(index%width, index/width, color.NRGBA{
			R: pixels[index*3], G: pixels[index*3+1], B: pixels[index*3+2], A: 255,
		})
	}
	return result, nil
}

func readPPMInt(reader *bufio.Reader) (int, error) {
	value, err := readPPMToken(reader)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid PPM integer")
	}
	return parsed, nil
}

func readPPMToken(reader *bufio.Reader) (string, error) {
	var token []byte
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		if value == '#' && len(token) == 0 {
			if _, err := reader.ReadString('\n'); err != nil {
				return "", err
			}
			continue
		}
		if value == ' ' || value == '\t' || value == '\r' || value == '\n' {
			if len(token) == 0 {
				continue
			}
			return string(token), nil
		}
		if len(token) >= 32 {
			return "", fmt.Errorf("PPM token exceeds limit")
		}
		token = append(token, value)
	}
}
