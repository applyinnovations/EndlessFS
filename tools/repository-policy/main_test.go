package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPolicies(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writePolicy(t, directory, "main.json", `{
  "name": "Protect main",
  "target": "branch",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "rules": [{"type": "deletion"}]
}`)
	documents, err := loadPolicies(directory)
	if err != nil {
		t.Fatalf("loadPolicies() error = %v", err)
	}
	if len(documents) != 1 || documents[0].policy.Name != "Protect main" {
		t.Fatalf("loadPolicies() = %+v", documents)
	}
}

func TestLoadPoliciesAcceptsBypassActors(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writePolicy(t, directory, "release-creation.json", `{
  "name": "Restrict release creation",
  "target": "tag",
  "enforcement": "active",
  "bypass_actors": [{"actor_id": 7, "actor_type": "User", "bypass_mode": "always"}],
  "conditions": {"ref_name": {"include": ["refs/tags/v*.*.*"], "exclude": []}},
  "rules": [{"type": "creation"}]
}`)
	documents, err := loadPolicies(directory)
	if err != nil {
		t.Fatalf("loadPolicies() error = %v", err)
	}
	if len(documents) != 1 || !strings.Contains(string(documents[0].payload), `"bypass_actors"`) {
		t.Fatalf("loadPolicies() = %+v", documents)
	}
}

func TestLoadPoliciesRejectsInvalidBypassActors(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writePolicy(t, directory, "invalid.json", `{
  "name": "Invalid bypass actors",
  "target": "tag",
  "enforcement": "active",
  "bypass_actors": {"actor_id": 7},
  "conditions": {"ref_name": {"include": ["refs/tags/v*.*.*"], "exclude": []}},
  "rules": [{"type": "creation"}]
}`)
	if _, err := loadPolicies(directory); err == nil || !strings.Contains(err.Error(), "bypass_actors must be an array") {
		t.Fatalf("loadPolicies() error = %v", err)
	}
}

func TestLoadPoliciesRejectsUnknownAndTrailingFields(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		`{"name":"x","target":"branch","enforcement":"active","conditions":{},"rules":[],"surprise":true}`,
		`{"name":"x","target":"branch","enforcement":"active","conditions":{},"rules":[]} {}`,
	} {
		directory := t.TempDir()
		writePolicy(t, directory, "invalid.json", content)
		if _, err := loadPolicies(directory); err == nil {
			t.Fatalf("loadPolicies() accepted %q", content)
		}
	}
}

func TestApplyCreatesAndUpdatesWithoutLeakingToken(t *testing.T) {
	t.Parallel()

	const token = "test-secret-token"
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":7,"name":"Protect main","target":"branch"}]`))
		case r.Method == http.MethodPut, r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	directory := t.TempDir()
	writePolicy(t, directory, "main.json", `{"name":"Protect main","target":"branch","enforcement":"active","conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},"rules":[{"type":"deletion"}]}`)
	writePolicy(t, directory, "tags.json", `{"name":"Protect releases","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"rules":[{"type":"deletion"}]}`)
	documents, err := loadPolicies(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(server.Client(), server.URL, "owner/repo", token, documents); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	joined := strings.Join(methods, "\n")
	for _, want := range []string{
		"GET /repos/owner/repo/rulesets",
		"PUT /repos/owner/repo/rulesets/7",
		"POST /repos/owner/repo/rulesets",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("requests %q do not contain %q", joined, want)
		}
	}
}

func TestRequestRejectsUntrustedEndpoints(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"http://api.github.com/repos/owner/repo/rulesets",
		"https://example.com/repos/owner/repo/rulesets",
		"https://api.github.com:8443/repos/owner/repo/rulesets",
		"https://user@api.github.com/repos/owner/repo/rulesets",
	} {
		if _, err := request(http.DefaultClient, http.MethodGet, endpoint, "secret", nil); err == nil {
			t.Errorf("request() accepted %q", endpoint)
		}
	}
}

func writePolicy(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
