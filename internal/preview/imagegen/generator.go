// Package imagegen implements the closed v1.1 raster-image generator and its
// one-shot, hard-cancelable worker protocol. It emits static WebP only.
package imagegen

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"

	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/deepteams/webp"

	_ "image/gif"
	_ "image/jpeg"
)

const (
	recipeID             = "image-webp-q80-v1"
	defaultMaxPixels     = 40_000_000
	defaultMaxDimension  = 16_383
	defaultMaxSourceSize = int64(128 << 20)
)

type Options struct {
	MaxPixels      int
	MaxDimension   int
	MaxSourceBytes int64
}

type Generator struct {
	maxPixels      int
	maxDimension   int
	maxSourceBytes int64
}

func New(options Options) *Generator {
	if options.MaxPixels == 0 {
		options.MaxPixels = defaultMaxPixels
	}
	if options.MaxDimension == 0 {
		options.MaxDimension = defaultMaxDimension
	}
	if options.MaxSourceBytes == 0 {
		options.MaxSourceBytes = defaultMaxSourceSize
	}
	return &Generator{maxPixels: options.MaxPixels, maxDimension: options.MaxDimension, maxSourceBytes: options.MaxSourceBytes}
}

func (*Generator) Capability() string { return "image" }
func (*Generator) RecipeID() string   { return recipeID }

func (*Generator) Supports(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func (g *Generator) SelfTest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fixture := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	fixture.SetNRGBA(0, 0, color.NRGBA{R: 210, G: 20, B: 40, A: 91})
	fixture.SetNRGBA(1, 0, color.NRGBA{R: 10, G: 200, B: 80, A: 255})
	var source bytes.Buffer
	if err := png.Encode(&source, fixture); err != nil {
		return err
	}
	generated, err := g.Generate(ctx, preview.GenerationRequest{
		Source: bytes.NewReader(source.Bytes()), SourceSize: int64(source.Len()), MediaType: "image/png", Variant: 64,
	})
	if err != nil {
		return err
	}
	features, err := webp.GetFeatures(bytes.NewReader(generated.Bytes))
	if err != nil || features.Width != 2 || features.Height != 1 || !features.HasAlpha || features.HasAnimation || features.FrameCount != 1 {
		return fmt.Errorf("image generator integrity output is invalid")
	}
	return nil
}

func (g *Generator) Generate(ctx context.Context, request preview.GenerationRequest) (preview.GeneratedArtifact, error) {
	if err := ctx.Err(); err != nil {
		return preview.GeneratedArtifact{}, err
	}
	if request.Source == nil || !g.Supports(request.MediaType) || request.SourceSize < 1 || request.SourceSize > g.maxSourceBytes ||
		request.Variant < 64 || request.Variant > 4096 || g.maxPixels < 1 || g.maxDimension < 1 || g.maxSourceBytes < 1 {
		return preview.GeneratedArtifact{}, fmt.Errorf("image generator request is invalid")
	}
	data, err := io.ReadAll(io.LimitReader(request.Source, g.maxSourceBytes+1))
	if err != nil || int64(len(data)) != request.SourceSize || int64(len(data)) > g.maxSourceBytes {
		return preview.GeneratedArtifact{}, fmt.Errorf("image source byte limit exceeded")
	}
	configuration, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != expectedFormat(request.MediaType) {
		return preview.GeneratedArtifact{}, fmt.Errorf("image signature does not match configured media type")
	}
	if configuration.Width < 1 || configuration.Height < 1 || configuration.Width > g.maxDimension || configuration.Height > g.maxDimension ||
		configuration.Width > g.maxPixels/configuration.Height {
		return preview.GeneratedArtifact{}, fmt.Errorf("image dimensions exceed generator limits")
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		return preview.GeneratedArtifact{}, fmt.Errorf("image decode failed")
	}
	if err := ctx.Err(); err != nil {
		return preview.GeneratedArtifact{}, err
	}
	oriented := applyOrientation(decoded, sourceOrientation(request.MediaType, data))
	resized := resizeToMaximumEdge(oriented, request.Variant)
	options := webp.DefaultOptions()
	options.Lossless = false
	options.Quality = 80
	options.Method = 4
	options.AlphaCompression = 1
	options.AlphaQuality = 100
	options.ICC = nil
	options.EXIF = nil
	options.XMP = nil
	var output bytes.Buffer
	if err := webp.Encode(&output, resized, options); err != nil {
		return preview.GeneratedArtifact{}, fmt.Errorf("WebP encode failed")
	}
	if err := ctx.Err(); err != nil {
		return preview.GeneratedArtifact{}, err
	}
	bounds := resized.Bounds()
	return preview.GeneratedArtifact{Bytes: output.Bytes(), Width: bounds.Dx(), Height: bounds.Dy()}, nil
}

func expectedFormat(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpeg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

func resizeToMaximumEdge(source image.Image, maximum int) *image.NRGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= maximum && height <= maximum {
		return copyNRGBA(source)
	}
	scale := math.Min(float64(maximum)/float64(width), float64(maximum)/float64(height))
	destinationWidth := max(1, int(math.Round(float64(width)*scale)))
	destinationHeight := max(1, int(math.Round(float64(height)*scale)))
	destination := image.NewNRGBA(image.Rect(0, 0, destinationWidth, destinationHeight))
	for y := range destinationHeight {
		sourceY := (float64(y)+0.5)*float64(height)/float64(destinationHeight) - 0.5
		y0 := max(0, min(height-1, int(math.Floor(sourceY))))
		y1 := min(height-1, y0+1)
		weightY := sourceY - math.Floor(sourceY)
		for x := range destinationWidth {
			sourceX := (float64(x)+0.5)*float64(width)/float64(destinationWidth) - 0.5
			x0 := max(0, min(width-1, int(math.Floor(sourceX))))
			x1 := min(width-1, x0+1)
			weightX := sourceX - math.Floor(sourceX)
			destination.SetNRGBA(x, y, interpolate(
				nrgbaAt(source, bounds.Min.X+x0, bounds.Min.Y+y0), nrgbaAt(source, bounds.Min.X+x1, bounds.Min.Y+y0),
				nrgbaAt(source, bounds.Min.X+x0, bounds.Min.Y+y1), nrgbaAt(source, bounds.Min.X+x1, bounds.Min.Y+y1),
				weightX, weightY,
			))
		}
	}
	return destination
}

func copyNRGBA(source image.Image) *image.NRGBA {
	bounds := source.Bounds()
	destination := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := range bounds.Dy() {
		for x := range bounds.Dx() {
			destination.SetNRGBA(x, y, nrgbaAt(source, bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return destination
}

func nrgbaAt(source image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(source.At(x, y)).(color.NRGBA)
}

func interpolate(topLeft, topRight, bottomLeft, bottomRight color.NRGBA, weightX, weightY float64) color.NRGBA {
	weights := [4]float64{(1 - weightX) * (1 - weightY), weightX * (1 - weightY), (1 - weightX) * weightY, weightX * weightY}
	colors := [4]color.NRGBA{topLeft, topRight, bottomLeft, bottomRight}
	var red, green, blue, alpha float64
	for index, value := range colors {
		weight := weights[index]
		alphaValue := float64(value.A) / 255
		red += float64(value.R) * alphaValue * weight
		green += float64(value.G) * alphaValue * weight
		blue += float64(value.B) * alphaValue * weight
		alpha += float64(value.A) * weight
	}
	if alpha <= 0 {
		return color.NRGBA{}
	}
	alphaFraction := alpha / 255
	return color.NRGBA{
		R: uint8(math.Round(max(0, min(255, red/alphaFraction)))),
		G: uint8(math.Round(max(0, min(255, green/alphaFraction)))),
		B: uint8(math.Round(max(0, min(255, blue/alphaFraction)))),
		A: uint8(math.Round(max(0, min(255, alpha)))),
	}
}

func applyOrientation(source image.Image, orientation int) *image.NRGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	destinationWidth, destinationHeight := width, height
	if orientation >= 5 && orientation <= 8 {
		destinationWidth, destinationHeight = height, width
	}
	destination := image.NewNRGBA(image.Rect(0, 0, destinationWidth, destinationHeight))
	for y := range destinationHeight {
		for x := range destinationWidth {
			sourceX, sourceY := x, y
			switch orientation {
			case 2:
				sourceX = width - 1 - x
			case 3:
				sourceX, sourceY = width-1-x, height-1-y
			case 4:
				sourceY = height - 1 - y
			case 5:
				sourceX, sourceY = y, x
			case 6:
				sourceX, sourceY = y, height-1-x
			case 7:
				sourceX, sourceY = width-1-y, height-1-x
			case 8:
				sourceX, sourceY = width-1-y, x
			}
			destination.SetNRGBA(x, y, nrgbaAt(source, bounds.Min.X+sourceX, bounds.Min.Y+sourceY))
		}
	}
	return destination
}

func sourceOrientation(mediaType string, data []byte) int {
	switch mediaType {
	case "image/jpeg":
		return jpegOrientation(data)
	case "image/png":
		return pngOrientation(data)
	case "image/webp":
		return webpOrientation(data)
	default:
		return 1
	}
}

func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != 0xff {
			return 1
		}
		marker := data[offset+1]
		if marker == 0xda || marker == 0xd9 {
			return 1
		}
		size := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if size < 2 || offset+2+size > len(data) {
			return 1
		}
		payload := data[offset+4 : offset+2+size]
		if marker == 0xe1 && len(payload) >= 6 && bytes.Equal(payload[:6], []byte("Exif\x00\x00")) {
			return tiffOrientation(payload[6:])
		}
		offset += 2 + size
	}
	return 1
}

func pngOrientation(data []byte) int {
	if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return 1
	}
	for offset := 8; offset+12 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if size < 0 || offset+12+size > len(data) {
			return 1
		}
		if string(data[offset+4:offset+8]) == "eXIf" {
			return tiffOrientation(data[offset+8 : offset+8+size])
		}
		offset += 12 + size
	}
	return 1
}

func webpOrientation(data []byte) int {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 1
	}
	for offset := 12; offset+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		end := offset + 8 + size
		if size < 0 || end > len(data) {
			return 1
		}
		if string(data[offset:offset+4]) == "EXIF" {
			payload := data[offset+8 : end]
			if len(payload) >= 6 && bytes.Equal(payload[:6], []byte("Exif\x00\x00")) {
				payload = payload[6:]
			}
			return tiffOrientation(payload)
		}
		offset = end + size%2
	}
	return 1
}

func tiffOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(data[2:4]) != 42 {
		return 1
	}
	offset := int(order.Uint32(data[4:8]))
	if offset < 0 || offset+2 > len(data) {
		return 1
	}
	count := int(order.Uint16(data[offset : offset+2]))
	for index := range count {
		entry := offset + 2 + index*12
		if entry+12 > len(data) {
			return 1
		}
		if order.Uint16(data[entry:entry+2]) == 0x0112 && order.Uint16(data[entry+2:entry+4]) == 3 && order.Uint32(data[entry+4:entry+8]) == 1 {
			orientation := int(order.Uint16(data[entry+8 : entry+10]))
			if orientation >= 1 && orientation <= 8 {
				return orientation
			}
			return 1
		}
	}
	return 1
}
