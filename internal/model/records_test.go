package model

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestProfileContainsExactlyIdentityFields(t *testing.T) {
	t.Parallel()

	userID := mustUserID(t, 0x11)
	displayName, _ := domain.ParseDisplayName("Same Name")
	encoded, err := state.EncodeJSON(&Profile{UserID: userID, DisplayName: displayName})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["userID"] == nil || fields["displayName"] == nil {
		t.Fatalf("profile fields = %v", fields)
	}
	var decoded Profile
	if err := state.DecodeJSON(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.UserID != userID || decoded.DisplayName.String() != "Same Name" {
		t.Fatalf("decoded profile = %+v", decoded)
	}
}

func TestThemePreferenceIsSeparateAndStrict(t *testing.T) {
	t.Parallel()

	for _, themeID := range []string{"system", "endlessfs-light", "custom-1"} {
		record := &ThemePreference{SchemaVersion: SchemaVersion, ThemeID: themeID}
		if _, err := state.EncodeJSON(record); err != nil {
			t.Fatalf("theme %q: %v", themeID, err)
		}
	}
	for _, themeID := range []string{"", "../escape", "Remote URL", "UPPER"} {
		if _, err := state.EncodeJSON(&ThemePreference{SchemaVersion: SchemaVersion, ThemeID: themeID}); err == nil {
			t.Fatalf("accepted theme ID %q", themeID)
		}
	}
}

func TestRecordDecoderRejectsFieldsOutsideSchema(t *testing.T) {
	t.Parallel()

	userID := mustUserID(t, 0x22)
	data := []byte(`{"userID":"` + userID.String() + `","displayName":"User","themeID":"system"}`)
	var profile Profile
	if err := state.DecodeJSON(data, &profile); err == nil {
		t.Fatal("profile accepted presentation state")
	}
}

func mustUserID(t *testing.T, value byte) domain.UserID {
	t.Helper()
	encoded := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, 16))
	userID, err := domain.ParseUserID(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return userID
}
