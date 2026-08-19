package theme

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"strconv"
	"strings"
)

type ValidatedAsset struct {
	Path        string `json:"path"`
	Digest      string `json:"digest"`
	ContentType string `json:"contentType"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Data        []byte `json:"-"`
}

func validateMedia(name string, data []byte, slot MediaSlot) (ValidatedAsset, error) {
	if int64(len(data)) > slot.MaximumBytes || len(data) == 0 {
		return ValidatedAsset{}, fmt.Errorf("asset %q has an invalid size", name)
	}
	extension := strings.ToLower(path.Ext(name))
	contentType := ""
	width, height := 0, 0
	var err error
	switch extension {
	case ".svg":
		contentType = "image/svg+xml"
		width, height, err = sanitizeSVG(data)
	case ".png":
		contentType = "image/png"
		width, height, err = decodeRaster(data, "png")
	case ".webp":
		contentType = "image/webp"
		width, height, err = decodeWebP(data)
	case ".avif":
		contentType = "image/avif"
		width, height, err = decodeAVIF(data)
	default:
		return ValidatedAsset{}, fmt.Errorf("asset %q has a forbidden media format", name)
	}
	if err != nil || !contains(slot.Accepted, contentType) {
		return ValidatedAsset{}, fmt.Errorf("asset %q failed media validation", name)
	}
	if width < 1 || height < 1 || width > slot.MaximumDimension || height > slot.MaximumDimension || int64(width)*int64(height) > slot.MaximumPixels {
		return ValidatedAsset{}, fmt.Errorf("asset %q exceeds decoded dimension limits", name)
	}
	digest := sha256.Sum256(data)
	return ValidatedAsset{Path: name, Digest: base64.RawURLEncoding.EncodeToString(digest[:]), ContentType: contentType, Width: width, Height: height, Data: append([]byte(nil), data...)}, nil
}

func decodeRaster(data []byte, expected string) (int, int, error) {
	configuration, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != expected {
		return 0, 0, fmt.Errorf("invalid raster signature")
	}
	return configuration.Width, configuration.Height, nil
}

func decodeWebP(data []byte) (int, int, error) {
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("invalid WebP signature")
	}
	switch string(data[12:16]) {
	case "VP8X":
		return 1 + int(uint32(data[24])|uint32(data[25])<<8|uint32(data[26])<<16), 1 + int(uint32(data[27])|uint32(data[28])<<8|uint32(data[29])<<16), nil
	case "VP8 ":
		if len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, fmt.Errorf("invalid VP8 frame")
		}
		return int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff), nil
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, fmt.Errorf("invalid VP8L frame")
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return 1 + int(bits&0x3fff), 1 + int((bits>>14)&0x3fff), nil
	default:
		return 0, 0, fmt.Errorf("unsupported WebP encoding")
	}
}

func decodeAVIF(data []byte) (int, int, error) {
	if len(data) < 16 || string(data[4:8]) != "ftyp" || (string(data[8:12]) != "avif" && string(data[8:12]) != "avis") {
		return 0, 0, fmt.Errorf("invalid AVIF signature")
	}
	for offset := 0; offset+20 <= len(data); offset++ {
		if string(data[offset:offset+4]) == "ispe" {
			width := int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
			height := int(binary.BigEndian.Uint32(data[offset+12 : offset+16]))
			if width > 0 && height > 0 {
				return width, height, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("AVIF dimensions are missing")
}

var allowedSVGElements = map[string]bool{
	"svg": true, "g": true, "path": true, "circle": true, "ellipse": true, "rect": true, "line": true, "polyline": true, "polygon": true, "defs": true, "linearGradient": true, "radialGradient": true, "stop": true, "clipPath": true, "mask": true, "title": true, "desc": true,
}
var allowedSVGAttributes = map[string]bool{
	"xmlns": true, "version": true, "width": true, "height": true, "viewBox": true, "x": true, "y": true, "x1": true, "y1": true, "x2": true, "y2": true, "cx": true, "cy": true, "r": true, "rx": true, "ry": true, "d": true, "points": true, "fill": true, "fill-rule": true, "stroke": true, "stroke-width": true, "stroke-linecap": true, "stroke-linejoin": true, "opacity": true, "fill-opacity": true, "stroke-opacity": true, "transform": true, "offset": true, "stop-color": true, "stop-opacity": true, "gradientUnits": true, "gradientTransform": true, "id": true, "clip-path": true, "mask": true, "preserveAspectRatio": true, "role": true, "aria-hidden": true, "focusable": true,
}

func sanitizeSVG(data []byte) (int, int, error) {
	if bytes.Contains(bytes.ToLower(data), []byte("<!doctype")) || bytes.Contains(bytes.ToLower(data), []byte("<!entity")) {
		return 0, 0, fmt.Errorf("SVG declarations are forbidden")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	depth := 0
	rootSeen := false
	width, height := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if depth > 64 || !allowedSVGElements[value.Name.Local] {
				return 0, 0, fmt.Errorf("active or unsupported SVG element")
			}
			if !rootSeen {
				if value.Name.Local != "svg" {
					return 0, 0, fmt.Errorf("SVG root is required")
				}
				rootSeen = true
			}
			for _, attribute := range value.Attr {
				name := attribute.Name.Local
				lowerValue := strings.ToLower(attribute.Value)
				external := name != "xmlns" && (strings.Contains(lowerValue, "url(") || strings.Contains(lowerValue, "data:") || strings.Contains(lowerValue, "http:") || strings.Contains(lowerValue, "https:") || strings.Contains(lowerValue, "//"))
				if strings.HasPrefix(strings.ToLower(name), "on") || !allowedSVGAttributes[name] || name == "style" || name == "href" || name == "src" || external {
					return 0, 0, fmt.Errorf("active or unsupported SVG attribute")
				}
				if depth == 1 && name == "width" {
					width, err = svgDimension(attribute.Value)
					if err != nil {
						return 0, 0, err
					}
				}
				if depth == 1 && name == "height" {
					height, err = svgDimension(attribute.Value)
					if err != nil {
						return 0, 0, err
					}
				}
				if depth == 1 && name == "viewBox" && (width == 0 || height == 0) {
					fields := strings.Fields(attribute.Value)
					if len(fields) != 4 {
						return 0, 0, fmt.Errorf("invalid SVG viewBox")
					}
					viewWidth, parseWidth := strconv.ParseFloat(fields[2], 64)
					viewHeight, parseHeight := strconv.ParseFloat(fields[3], 64)
					if parseWidth != nil || parseHeight != nil || viewWidth <= 0 || viewHeight <= 0 {
						return 0, 0, fmt.Errorf("invalid SVG viewBox")
					}
					if width == 0 {
						width = int(viewWidth)
					}
					if height == 0 {
						height = int(viewHeight)
					}
				}
			}
		case xml.EndElement:
			depth--
		case xml.Directive, xml.ProcInst:
			return 0, 0, fmt.Errorf("SVG directives are forbidden")
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return 0, 0, fmt.Errorf("SVG text content is forbidden")
			}
		}
	}
	if !rootSeen || depth != 0 || width < 1 || height < 1 {
		return 0, 0, fmt.Errorf("invalid SVG dimensions")
	}
	return width, height, nil
}

func svgDimension(value string) (int, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 || parsed != float64(int(parsed)) {
		return 0, fmt.Errorf("SVG dimensions must be positive integer pixels")
	}
	return int(parsed), nil
}

func validateSprite(reference AssetReference, asset ValidatedAsset) error {
	if !reference.Sprite {
		return nil
	}
	if asset.ContentType == "image/svg+xml" || reference.X < 0 || reference.Y < 0 || reference.Width < 1 || reference.Height < 1 || reference.PixelRatio < 1 || reference.PixelRatio > 4 || reference.X+reference.Width > asset.Width || reference.Y+reference.Height > asset.Height {
		return fmt.Errorf("invalid raster sprite rectangle")
	}
	return nil
}
