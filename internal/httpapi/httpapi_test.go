package httpapi

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/config"
	endlesslogging "github.com/applyinnovations/endlessfs/internal/logging"
)

func TestIntegrationFaviconServesEmbeddedMultiSizeICO(t *testing.T) {
	handler := New(config.PublicConfig{}, "test-version")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/vnd.microsoft.icon" {
		t.Fatalf("favicon response = %d %q", response.Code, response.Header().Get("Content-Type"))
	}
	assertSecurityHeaders(t, response.Header())
	data := response.Body.Bytes()
	if len(data) < 54 || binary.LittleEndian.Uint16(data[:2]) != 0 || binary.LittleEndian.Uint16(data[2:4]) != 1 || binary.LittleEndian.Uint16(data[4:6]) != 3 {
		t.Fatal("favicon is not a three-image ICO")
	}
	for index, size := range []int{16, 32, 48} {
		entry := data[6+index*16 : 22+index*16]
		length, offset := int(binary.LittleEndian.Uint32(entry[8:12])), int(binary.LittleEndian.Uint32(entry[12:16]))
		if offset < 54 || length < 1 || offset > len(data)-length {
			t.Fatal("invalid ICO image extent")
		}
		image, err := png.Decode(bytes.NewReader(data[offset : offset+length]))
		if err != nil || image.Bounds().Dx() != size || image.Bounds().Dy() != size {
			t.Fatalf("favicon size %d did not decode: %v", size, err)
		}
		visible := false
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				_, _, _, alpha := image.At(x, y).RGBA()
				visible = visible || alpha > 0
			}
		}
		if !visible {
			t.Fatalf("favicon size %d is blank", size)
		}
	}
}

func TestIntegrationPublicEndpoints(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{InviteRegistration: true}, "test-version")
	tests := []struct {
		path        string
		contentType string
		body        string
	}{
		{path: "/healthz", contentType: "text/plain; charset=utf-8", body: "ok\n"},
		{path: "/readyz", contentType: "text/plain; charset=utf-8", body: "ready\n"},
		{path: "/", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/bootstrap", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/register", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/trash", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/settings", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/admin", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/assets/ui.css", contentType: "text/css; charset=utf-8", body: "color-scheme"},
		{path: "/assets/ui.js", contentType: "text/javascript; charset=utf-8", body: "addEventListener"},
		{path: "/assets/brand/endlessfs-mark.svg", contentType: "image/svg+xml", body: "<svg"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Content-Type"); got != test.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, test.contentType)
			}
			if !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("body %q does not contain %q", response.Body.String(), test.body)
			}
			if test.path == "/assets/brand/endlessfs-mark.svg" {
				if got := response.Header().Get("Content-Security-Policy"); got != "default-src 'none'; style-src 'none'; sandbox" {
					t.Fatalf("isolated SVG CSP = %q", got)
				}
			} else {
				assertSecurityHeaders(t, response.Header())
			}
		})
	}
}

func TestIntegrationPublicConfigExposesNoSecrets(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{AllowRegistration: true, InviteRegistration: false}, "v0-test")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if value["product"] != "EndlessFS" || value["version"] != "v0-test" {
		t.Fatalf("response = %#v", value)
	}
	if value["allowRegistration"] != true || value["inviteRegistration"] != false {
		t.Fatalf("response = %#v", value)
	}
	capabilities, ok := value["previewCapabilities"].(map[string]any)
	accepted, acceptedOK := capabilities["acceptedImageMediaTypes"].([]any)
	decoders, decodersOK := capabilities["packagedImageDecoders"].([]any)
	if !ok || capabilities["previewSpecification"] != "v1.1" || capabilities["profile"] != "images" || capabilities["artifactMediaTypes"].([]any)[0] != "image/webp" ||
		!acceptedOK || len(accepted) != 13 || accepted[12] != "image/x-sony-arw" || !decodersOK || len(decoders) != 3 || decoders[2] != "libraw-0.22.1" {
		t.Fatalf("preview capability manifest = %#v", value["previewCapabilities"])
	}
	for key := range value {
		if strings.Contains(strings.ToLower(key), "secret") || strings.Contains(strings.ToLower(key), "token") {
			t.Fatalf("secret-shaped key exposed: %q", key)
		}
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestIntegrationMutatingPublicRouteIsNotAccepted(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{}, "test")
	request := httptest.NewRequest(http.MethodPost, "/healthz", strings.NewReader("payload"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestUnknownPathDoesNotFallBackToApplicationShell(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{}, "test")
	request := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestRequestLoggingUsesRouteTemplatesAndOmitsSensitiveMaterial(t *testing.T) {
	t.Parallel()
	const marker = "secret-marker-must-not-be-logged"
	var output bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /public/shares/{token}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	})
	handler := requestIDMiddleware(requestLogMiddleware(mux, endlesslogging.NewJSON(&output, slog.LevelInfo)))
	request := httptest.NewRequest(http.MethodGet, "/public/shares/"+marker+"?capability="+marker, nil)
	request.Header.Set("Authorization", "Bearer "+marker)
	request.Header.Set("Cookie", "endlessfs_session="+marker)
	request.Header.Set("X-Request-ID", "safe-request-id")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	got := output.String()
	if strings.Contains(got, marker) {
		t.Fatalf("request log leaked request material: %s", got)
	}
	for _, required := range []string{
		`"msg":"request_completed"`,
		`"requestID":"safe-request-id"`,
		`"route":"GET /public/shares/{token}"`,
		`"status":410`,
		`"result":"client_error"`,
		`"duration":`,
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("request log missing %s: %s", required, got)
		}
	}
}

func assertSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{
		"Content-Security-Policy",
		"Cross-Origin-Opener-Policy",
		"Permissions-Policy",
		"Referrer-Policy",
		"X-Content-Type-Options",
	} {
		if header.Get(name) == "" {
			t.Errorf("%s is missing", name)
		}
	}
	if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self'") {
		t.Errorf("Content-Security-Policy does not constrain preview frames: %q", csp)
	}
	if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' blob:") {
		t.Errorf("Content-Security-Policy does not allow validated in-memory preview images: %q", csp)
	}
	if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "worker-src 'self'") {
		t.Errorf("Content-Security-Policy does not constrain upload hash workers: %q", csp)
	}
	if csp := header.Get("Content-Security-Policy"); strings.Contains(csp, "'unsafe-inline'") {
		t.Errorf("Content-Security-Policy permits inline styles or scripts: %q", csp)
	}
	if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src-attr 'none'") || strings.Count(csp, "blob:") != 1 {
		t.Errorf("Content-Security-Policy does not keep preview blobs and inline styles narrowly scoped: %q", csp)
	}
}
