package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDomainErrorsExposeStableKindsWithoutInternalCauses(t *testing.T) {
	cause := errors.New("internal detail")
	for kind, sentinelError := range map[ErrorKind]error{
		ErrorInvalid: ErrInvalid, ErrorUnauthenticated: ErrUnauthenticated, ErrorUnauthorized: ErrUnauthorized,
		ErrorNotFound: ErrNotFound, ErrorConflict: ErrConflict, ErrorPreconditionFailed: ErrPreconditionFailed,
		ErrorRateLimited: ErrRateLimited, ErrorUnavailable: ErrUnavailable, ErrorInternal: ErrInternal,
	} {
		t.Run(string(kind), func(t *testing.T) {
			plain := NewError(kind, "safe")
			if KindOf(plain) != kind || !errors.Is(plain, sentinelError) || plain.Error() != string(kind)+": safe" {
				t.Fatalf("plain error = %v kind=%v", plain, KindOf(plain))
			}
			wrapped := WrapError(kind, "safe", cause)
			if !errors.Is(wrapped, cause) || strings.Contains(wrapped.Error(), cause.Error()) {
				t.Fatalf("wrapped error leaked or lost cause: %v", wrapped)
			}
		})
	}
	if got := (&Error{Kind: ErrorInvalid}).Error(); got != "invalid" {
		t.Fatalf("empty-message error = %q", got)
	}
	if KindOf(fmt.Errorf("wrapped: %w", ErrNotFound)) != ErrorNotFound || KindOf(errors.New("other")) != ErrorInternal {
		t.Fatal("KindOf did not classify sentinel and unknown errors")
	}
}

func TestDomainJSONAccessorsAndNormalizers(t *testing.T) {
	rawID := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 16))
	userID, err := ParseUserID(rawID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(userID)
	if err != nil {
		t.Fatal(err)
	}
	var decoded UserID
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != userID {
		t.Fatalf("user ID JSON = %v, %v", decoded, err)
	}
	for _, value := range []string{"", "%%%", base64.RawURLEncoding.EncodeToString(make([]byte, 15)), rawID + "="} {
		if _, err := ParseUserID(value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseUserID(%q) = %v", value, err)
		}
	}
	if _, err := json.Marshal(UserID{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid user ID marshal = %v", err)
	}
	if err := json.Unmarshal([]byte(`123`), &decoded); err == nil {
		t.Fatal("numeric user ID decoded")
	}

	scope, _ := NewScope(userID, AreaTrash)
	if !scope.Valid() || scope.UserID() != userID || scope.Area() != AreaTrash || (Scope{}).Valid() {
		t.Fatalf("scope accessors = %#v", scope)
	}
	generator := NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x55}, 64)))
	if opaque, err := generator.OpaqueID(); err != nil || len(opaque) != 22 {
		t.Fatalf("OpaqueID() = %q, %v", opaque, err)
	}
	if now := (SystemClock{}).Now(); now.Location() != time.UTC {
		t.Fatalf("system clock location = %v", now.Location())
	}
	if SystemIDGenerator() == nil {
		t.Fatal("system ID generator is nil")
	}

	path := MustParseUserPath("/folder/file.txt")
	if got := path.Segments(); len(got) != 2 || path.Name() != "file.txt" || !strings.Contains(path.GoString(), "folder") {
		t.Fatalf("path accessors = %v %q %s", got, path.Name(), path.GoString())
	}
	if (UserPath{}).Segments() != nil || (UserPath{}).Name() != "" || MustParseUserPath("/").Segments() != nil {
		t.Fatal("invalid/root path accessors were not empty")
	}
	for _, segment := range []string{"", ".", "..", "bad\\name", ".trash"} {
		if _, err := MustParseUserPath("/").Join(segment); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Join(%q) = %v", segment, err)
		}
	}
	if _, err := (UserPath{}).Join("name"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid base Join = %v", err)
	}
	if _, err := MustParseUserPath("/").Join(string([]byte{0xff})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid UTF-8 Join = %v", err)
	}
	if _, err := json.Marshal(UserPath{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid path marshal = %v", err)
	}
	decomposed, err := MustParseUserPath("/folder").Join("Cafe\u0301")
	if err != nil || decomposed.String() != "/folder/Café" {
		t.Fatalf("normalized Join = %q, %v", decomposed.String(), err)
	}

	display, _ := ParseDisplayName("Display")
	label, _ := ParseCredentialLabel("Key")
	for name, value := range map[string]any{"display": display, "label": label, "path": path} {
		data, err := json.Marshal(value)
		if err != nil || len(data) == 0 {
			t.Fatalf("marshal %s = %q, %v", name, data, err)
		}
	}
	var decodedDisplay DisplayName
	var decodedLabel CredentialLabel
	if err := json.Unmarshal([]byte(`" Display "`), &decodedDisplay); err != nil || decodedDisplay.String() != "Display" {
		t.Fatalf("display JSON = %q, %v", decodedDisplay, err)
	}
	if err := json.Unmarshal([]byte(`" Key "`), &decodedLabel); err != nil || decodedLabel.String() != "Key" {
		t.Fatalf("label JSON = %q, %v", decodedLabel, err)
	}
	for _, destination := range []any{&decodedDisplay, &decodedLabel} {
		if err := json.Unmarshal([]byte(`123`), destination); err == nil {
			t.Fatal("human label accepted a non-string")
		}
	}
	if _, err := json.Marshal(DisplayName{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid display marshal = %v", err)
	}
	if _, err := json.Marshal(CredentialLabel{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid label marshal = %v", err)
	}

	for _, mode := range []ConflictMode{ConflictFail, ConflictReplace, ConflictRename} {
		if !mode.Valid() {
			t.Fatalf("valid conflict mode %q rejected", mode)
		}
	}
	if normalized, err := NormalizeConflictMode(""); err != nil || normalized != ConflictFail {
		t.Fatalf("default conflict = %q, %v", normalized, err)
	}
	if _, err := NormalizeConflictMode("overwrite"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid conflict = %v", err)
	}
	for input, expected := range map[string]string{"Text/Plain; charset=utf-8": "text/plain", "IMAGE/PNG": "image/png"} {
		if got, err := NormalizeMediaType(input); err != nil || got != expected {
			t.Fatalf("NormalizeMediaType(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "plain", "bad\r\ntype", strings.Repeat("x", 256)} {
		if _, err := NormalizeMediaType(input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NormalizeMediaType(%q) = %v", input, err)
		}
	}
}

func TestMustParseUserPathPanicsForInvalidTrustedConstant(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustParseUserPath did not panic")
		}
	}()
	_ = MustParseUserPath("relative")
}
