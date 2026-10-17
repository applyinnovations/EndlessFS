package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestIntegrationDirectoryPreparationResponseExpansionIsSafeBeforePublication(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	paths := []string{"/ready"}
	for index := 1; index < domain.MaxDirectoryPreparationItems; index++ {
		// JSON.stringify preserves these characters. The Go JSON encoder's
		// default HTML escaping expands them sixfold in the response.
		path := fmt.Sprintf("/missing-%02d", index) + strings.Repeat("/"+strings.Repeat("<", 240), 16)
		if _, err := domain.ParseUserPath(path); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	body := `{"paths":["` + strings.Join(paths, `","`) + `"]}`
	if len(body) >= 1<<20 {
		t.Fatalf("fixture exceeds request bound: %d", len(body))
	}
	response := performRequest(t, env.handler, http.MethodPost, "/api/v1/directories/prepare", "https://drive.example.test", body,
		[]*http.Cookie{env.session, env.csrf}, driveMutationHeaders(env.csrf.Value, ""))
	switch response.Code {
	case http.StatusOK:
		if response.Body.Len() > 1<<20 {
			t.Fatalf("successful response exceeded bound: %d", response.Body.Len())
		}
		var result struct {
			Directories []domain.DirectoryPreparationResult `json:"directories"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Directories) != len(paths) || !result.Directories[0].Ready {
			t.Fatalf("incomplete bounded result: %+v", result)
		}
		for index, item := range result.Directories {
			if item.Index != index || item.Path.String() != paths[index] {
				t.Fatalf("misbound item %d: %+v", index, item)
			}
		}
	case http.StatusBadRequest:
		scope, err := domain.NewScope(env.user, domain.AreaLive)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.storage.Stat(context.Background(), scope, domain.MustParseUserPath("/ready")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("response bound rejection published a directory: %v", err)
		}
	default:
		t.Fatalf("valid bounded request became an ambiguous or unrecoverable response: status=%d request-bytes=%d response=%s", response.Code, len(body), response.Body.String())
	}
}
