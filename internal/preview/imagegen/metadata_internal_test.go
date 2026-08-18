package imagegen

import (
	"encoding/binary"
	"testing"
)

func TestMalformedOrientationMetadataFailsClosed(t *testing.T) {
	jpegInputs := [][]byte{
		{},
		{0xff, 0xd8, 0x00, 0x00},
		{0xff, 0xd8, 0xff, 0xd9},
		{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x01},
		{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x20},
		{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x02},
		{0xff, 0xd8, 0xff, 0xe1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0},
	}
	for _, data := range jpegInputs {
		if orientation := jpegOrientation(data); orientation != 1 {
			t.Fatalf("JPEG orientation = %d for %x", orientation, data)
		}
	}
	pngHeader := []byte("\x89PNG\r\n\x1a\n")
	pngOversize := append(append([]byte(nil), pngHeader...), 0xff, 0xff, 0xff, 0xff, 'e', 'X', 'I', 'f', 0, 0, 0, 0)
	for _, data := range [][]byte{{}, pngOversize, append(append([]byte(nil), pngHeader...), 0, 0, 0, 0, 'e', 'X', 'I', 'f', 0, 0, 0, 0)} {
		if orientation := pngOrientation(data); orientation != 1 {
			t.Fatalf("PNG orientation = %d for %x", orientation, data)
		}
	}
	webpOversize := append([]byte("RIFF\x00\x00\x00\x00WEBPEXIF"), 0xff, 0xff, 0xff, 0x7f)
	webpEmptyEXIF := append([]byte("RIFF\x08\x00\x00\x00WEBPEXIF"), 0, 0, 0, 0)
	for _, data := range [][]byte{{}, webpOversize, webpEmptyEXIF} {
		if orientation := webpOrientation(data); orientation != 1 {
			t.Fatalf("WebP orientation = %d for %x", orientation, data)
		}
	}
	if expectedFormat("application/octet-stream") != "" {
		t.Fatal("unknown media type received an image format")
	}
}

func TestMalformedTIFFOrientationFailsClosed(t *testing.T) {
	badMagic := make([]byte, 8)
	copy(badMagic, "II")
	badOffset := append([]byte(nil), badMagic...)
	binary.LittleEndian.PutUint16(badOffset[2:4], 42)
	binary.LittleEndian.PutUint32(badOffset[4:8], 100)
	truncatedEntry := make([]byte, 10)
	copy(truncatedEntry, "II")
	binary.LittleEndian.PutUint16(truncatedEntry[2:4], 42)
	binary.LittleEndian.PutUint32(truncatedEntry[4:8], 8)
	binary.LittleEndian.PutUint16(truncatedEntry[8:10], 1)
	invalidOrientation := make([]byte, 26)
	copy(invalidOrientation, "II")
	binary.LittleEndian.PutUint16(invalidOrientation[2:4], 42)
	binary.LittleEndian.PutUint32(invalidOrientation[4:8], 8)
	binary.LittleEndian.PutUint16(invalidOrientation[8:10], 1)
	binary.LittleEndian.PutUint16(invalidOrientation[10:12], 0x0112)
	binary.LittleEndian.PutUint16(invalidOrientation[12:14], 3)
	binary.LittleEndian.PutUint32(invalidOrientation[14:18], 1)
	binary.LittleEndian.PutUint16(invalidOrientation[18:20], 9)
	noOrientation := append([]byte(nil), invalidOrientation...)
	binary.LittleEndian.PutUint16(noOrientation[10:12], 0x0100)
	for _, data := range [][]byte{{}, []byte("XX000000"), badMagic, badOffset, truncatedEntry, invalidOrientation, noOrientation} {
		if orientation := tiffOrientation(data); orientation != 1 {
			t.Fatalf("TIFF orientation = %d for %x", orientation, data)
		}
	}
}
