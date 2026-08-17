package imagegen_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	"github.com/deepteams/webp"
)

func TestGeneratorProducesStaticWebPAtSourceAspectRatio(t *testing.T) {
	generator := imagegen.New(imagegen.Options{})
	source := image.NewNRGBA(image.Rect(0, 0, 160, 80))
	for y := range 80 {
		for x := range 160 {
			alpha := uint8(255)
			if x == 0 && y == 0 {
				alpha = 73
			}
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 5), G: uint8(y * 9), B: 120, A: alpha})
		}
	}
	input := encodePNG(t, source)
	generated, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(input), SourceSize: int64(len(input)), MediaType: "image/png", Variant: 64})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Width != 64 || generated.Height != 32 {
		t.Fatalf("dimensions = %dx%d, want 64x32", generated.Width, generated.Height)
	}
	features, err := webp.GetFeatures(bytes.NewReader(generated.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if features.Width != 64 || features.Height != 32 || !features.HasAlpha || features.HasAnimation || features.FrameCount != 1 {
		t.Fatalf("WebP features = %+v", features)
	}
	decoded, err := webp.Decode(bytes.NewReader(generated.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha == 0 || alpha == 0xffff {
		t.Fatalf("lossless alpha = %d, want preserved intermediate alpha", alpha)
	}
	for _, forbidden := range []string{"EXIF", "ICCP", "XMP ", "ANIM", "ANMF"} {
		if bytes.Contains(generated.Bytes, []byte(forbidden)) {
			t.Fatalf("generated WebP contains forbidden %s chunk", forbidden)
		}
	}
}

func TestGeneratorSupportsClosedInputSetAndNeverUpscales(t *testing.T) {
	generator := imagegen.New(imagegen.Options{})
	pngInput := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 7, 5)))
	jpegInput := encodeJPEG(t, image.NewRGBA(image.Rect(0, 0, 7, 5)))
	gifInput := encodeGIF(t, image.NewPaletted(image.Rect(0, 0, 7, 5), color.Palette{color.Black, color.White}))

	inputs := []struct {
		name      string
		mediaType string
		data      []byte
	}{
		{name: "png", mediaType: "image/png", data: pngInput},
		{name: "jpeg", mediaType: "image/jpeg", data: jpegInput},
		{name: "gif", mediaType: "image/gif", data: gifInput},
	}
	firstWebP, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(pngInput), SourceSize: int64(len(pngInput)), MediaType: "image/png", Variant: 64})
	if err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, struct {
		name      string
		mediaType string
		data      []byte
	}{name: "webp", mediaType: "image/webp", data: firstWebP.Bytes})

	for _, test := range inputs {
		t.Run(test.name, func(t *testing.T) {
			generated, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(test.data), SourceSize: int64(len(test.data)), MediaType: test.mediaType, Variant: 64})
			if err != nil {
				t.Fatal(err)
			}
			if generated.Width != 7 || generated.Height != 5 {
				t.Fatalf("dimensions = %dx%d, want no-upscale 7x5", generated.Width, generated.Height)
			}
			features, err := webp.GetFeatures(bytes.NewReader(generated.Bytes))
			if err != nil || features.HasAnimation {
				t.Fatalf("features = %+v, %v", features, err)
			}
		})
	}
	if generator.Supports("application/pdf") || generator.Supports("video/mp4") || generator.Supports("image/svg+xml") {
		t.Fatal("image generator accepted an unpackaged input format")
	}
}

func TestGeneratorFlattensAnimatedGIFToStaticWebP(t *testing.T) {
	first := image.NewPaletted(image.Rect(0, 0, 4, 2), color.Palette{color.Black, color.White})
	second := image.NewPaletted(image.Rect(0, 0, 4, 2), color.Palette{color.Black, color.White})
	for index := range second.Pix {
		second.Pix[index] = 1
	}
	var input bytes.Buffer
	if err := gif.EncodeAll(&input, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	generated, err := imagegen.New(imagegen.Options{}).Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(input.Bytes()), SourceSize: int64(input.Len()), MediaType: "image/gif", Variant: 64})
	if err != nil {
		t.Fatal(err)
	}
	features, err := webp.GetFeatures(bytes.NewReader(generated.Bytes))
	if err != nil || features.HasAnimation || features.FrameCount != 1 || features.Width != 4 || features.Height != 2 {
		t.Fatalf("flattened GIF features = %+v, %v", features, err)
	}
}

func TestGeneratorNormalizesJPEGOrientationAndStripsMetadata(t *testing.T) {
	generator := imagegen.New(imagegen.Options{})
	source := image.NewRGBA(image.Rect(0, 0, 3, 2))
	source.Set(0, 0, color.RGBA{R: 255, A: 255})
	oriented := addJPEGOrientation(t, encodeJPEG(t, source), 6)
	generated, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(oriented), SourceSize: int64(len(oriented)), MediaType: "image/jpeg", Variant: 64})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Width != 2 || generated.Height != 3 {
		t.Fatalf("oriented dimensions = %dx%d, want 2x3", generated.Width, generated.Height)
	}
	if bytes.Contains(generated.Bytes, []byte("Exif")) || bytes.Contains(generated.Bytes, []byte("EXIF")) {
		t.Fatal("generated preview retained EXIF metadata")
	}
}

func TestGeneratorNormalizesEveryJPEGOrientation(t *testing.T) {
	generator := imagegen.New(imagegen.Options{})
	source := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for orientation := uint16(2); orientation <= 8; orientation++ {
		oriented := addJPEGOrientation(t, encodeJPEG(t, source), orientation)
		generated, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(oriented), SourceSize: int64(len(oriented)), MediaType: "image/jpeg", Variant: 64})
		if err != nil {
			t.Fatalf("orientation %d: %v", orientation, err)
		}
		wantWidth, wantHeight := 3, 2
		if orientation >= 5 {
			wantWidth, wantHeight = 2, 3
		}
		if generated.Width != wantWidth || generated.Height != wantHeight {
			t.Fatalf("orientation %d dimensions = %dx%d", orientation, generated.Width, generated.Height)
		}
	}
}

func TestGeneratorRejectsMismatchedAndResourceExhaustingInputs(t *testing.T) {
	generator := imagegen.New(imagegen.Options{MaxPixels: 100, MaxDimension: 64, MaxSourceBytes: 1 << 20})
	large := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 11, 10)))
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(large), SourceSize: int64(len(large)), MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("generator accepted input above its decoded-pixel limit")
	}
	small := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(small), SourceSize: int64(len(small)), MediaType: "image/jpeg", Variant: 64}); err == nil {
		t.Fatal("generator accepted a media-type/signature mismatch")
	}
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: strings.NewReader("not an image"), SourceSize: 12, MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("generator accepted malformed image bytes")
	}
	if err := generator.SelfTest(context.Background()); err != nil {
		t.Fatalf("SelfTest() error = %v", err)
	}
}

func TestGeneratorCancellationReadAndDecodeFailures(t *testing.T) {
	generator := imagegen.New(imagegen.Options{})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := generator.Generate(canceled, preview.GenerationRequest{Source: strings.NewReader("x"), SourceSize: 1, MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("Generate accepted canceled context")
	}
	if err := generator.SelfTest(canceled); err == nil {
		t.Fatal("SelfTest accepted canceled context")
	}
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: errorReader{}, SourceSize: 1, MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("Generate ignored source read error")
	}
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{}); err == nil {
		t.Fatal("Generate accepted an empty request")
	}
	valid := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(valid), SourceSize: int64(len(valid) + 1), MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("Generate ignored declared source-size mismatch")
	}
	configurationOnly := valid[:33]
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(configurationOnly), SourceSize: int64(len(configurationOnly)), MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("Generate accepted configuration-only PNG")
	}
	readContext, cancelRead := context.WithCancel(context.Background())
	canceling := &cancelAfterReader{reader: bytes.NewReader(valid), cancel: cancelRead}
	if _, err := generator.Generate(readContext, preview.GenerationRequest{Source: canceling, SourceSize: int64(len(valid)), MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("Generate ignored cancellation after source read")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type cancelAfterReader struct {
	reader *bytes.Reader
	cancel context.CancelFunc
}

func (r *cancelAfterReader) Read(target []byte) (int, error) {
	count, err := r.reader.Read(target)
	if count > 0 {
		r.cancel()
	}
	return count, err
}

func TestGeneratorReadsPNGAndWebPOrientationMetadataAndTransparentPixels(t *testing.T) {
	generator := imagegen.New(imagegen.Options{})
	source := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	pngWithOrientation := addPNGOrientation(t, encodePNG(t, source), 6)
	pngResult, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(pngWithOrientation), SourceSize: int64(len(pngWithOrientation)), MediaType: "image/png", Variant: 64})
	if err != nil || pngResult.Width != 2 || pngResult.Height != 3 {
		t.Fatalf("PNG orientation result = %+v, %v", pngResult, err)
	}
	baseInput := encodePNG(t, source)
	baseWebP, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(baseInput), SourceSize: int64(len(baseInput)), MediaType: "image/png", Variant: 64})
	if err != nil {
		t.Fatal(err)
	}
	webpWithOrientation := addWebPOrientation(t, baseWebP.Bytes, 6)
	webpResult, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(webpWithOrientation), SourceSize: int64(len(webpWithOrientation)), MediaType: "image/webp", Variant: 64})
	if err != nil || webpResult.Width != 2 || webpResult.Height != 3 {
		t.Fatalf("WebP orientation result = %+v, %v", webpResult, err)
	}
	transparent := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	transparentInput := encodePNG(t, transparent)
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader(transparentInput), SourceSize: int64(len(transparentInput)), MediaType: "image/png", Variant: 64}); err != nil {
		t.Fatal(err)
	}
}

func FuzzGeneratorMalformed(f *testing.F) {
	f.Add([]byte("not-an-image"))
	f.Add(preview.OnePixelWebP())
	f.Fuzz(func(t *testing.T, data []byte) {
		generator := imagegen.New(imagegen.Options{MaxPixels: 4096, MaxDimension: 128, MaxSourceBytes: 1 << 20})
		if len(data) == 0 || len(data) > 1<<20 {
			return
		}
		_, _ = generator.Generate(context.Background(), preview.GenerationRequest{
			Source: bytes.NewReader(data), SourceSize: int64(len(data)), MediaType: "image/webp", Variant: 64,
		})
	})
}

func encodePNG(t *testing.T, value image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, value); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func encodeJPEG(t *testing.T, value image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, value, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func encodeGIF(t *testing.T, value *image.Paletted) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := gif.Encode(&output, value, nil); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func addJPEGOrientation(t *testing.T, source []byte, orientation uint16) []byte {
	t.Helper()
	if len(source) < 2 || source[0] != 0xff || source[1] != 0xd8 {
		t.Fatal("invalid JPEG fixture")
	}
	tiff := make([]byte, 26)
	copy(tiff[:2], "MM")
	binary.BigEndian.PutUint16(tiff[2:4], 42)
	binary.BigEndian.PutUint32(tiff[4:8], 8)
	binary.BigEndian.PutUint16(tiff[8:10], 1)
	binary.BigEndian.PutUint16(tiff[10:12], 0x0112)
	binary.BigEndian.PutUint16(tiff[12:14], 3)
	binary.BigEndian.PutUint32(tiff[14:18], 1)
	binary.BigEndian.PutUint16(tiff[18:20], orientation)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	result := append([]byte(nil), source[:2]...)
	result = append(result, segment...)
	result = append(result, payload...)
	result = append(result, source[2:]...)
	return result
}

func addPNGOrientation(t *testing.T, source []byte, orientation uint16) []byte {
	t.Helper()
	if len(source) < 20 || string(source[len(source)-8:len(source)-4]) != "IEND" {
		t.Fatal("invalid PNG fixture")
	}
	payload := littleEndianTIFFOrientation(orientation)
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], "eXIf")
	copy(chunk[8:8+len(payload)], payload)
	binary.BigEndian.PutUint32(chunk[8+len(payload):], crc32.ChecksumIEEE(chunk[4:8+len(payload)]))
	result := append([]byte(nil), source[:len(source)-12]...)
	result = append(result, chunk...)
	result = append(result, source[len(source)-12:]...)
	return result
}

func addWebPOrientation(t *testing.T, source []byte, orientation uint16) []byte {
	t.Helper()
	if len(source) < 21 || string(source[:4]) != "RIFF" || string(source[8:12]) != "WEBP" || string(source[12:16]) != "VP8X" {
		t.Fatal("invalid extended WebP fixture")
	}
	payload := littleEndianTIFFOrientation(orientation)
	chunk := make([]byte, 8+len(payload)+(len(payload)%2))
	copy(chunk[:4], "EXIF")
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(payload)))
	copy(chunk[8:], payload)
	result := append([]byte(nil), source...)
	result[20] |= 0x08
	result = append(result, chunk...)
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
	return result
}

func littleEndianTIFFOrientation(orientation uint16) []byte {
	tiff := make([]byte, 26)
	copy(tiff[:2], "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)
	binary.LittleEndian.PutUint16(tiff[12:14], 3)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint16(tiff[18:20], orientation)
	return tiff
}
