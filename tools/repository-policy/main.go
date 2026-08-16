// Command repository-policy validates or applies the checked-in GitHub
// repository rulesets. Applying policy is deliberately an explicit admin task.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const apiVersion = "2022-11-28"

type ruleset struct {
	Name        string          `json:"name"`
	Target      string          `json:"target"`
	Enforcement string          `json:"enforcement"`
	Conditions  json.RawMessage `json:"conditions"`
	Rules       json.RawMessage `json:"rules"`
}

type remoteRuleset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Target string `json:"target"`
}

type policyDocument struct {
	policy  ruleset
	payload []byte
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "check" && os.Args[1] != "apply") {
		fmt.Fprintln(os.Stderr, "usage: repository-policy check|apply")
		os.Exit(2)
	}

	documents, err := loadPolicies(".github/rulesets")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if os.Args[1] == "check" {
		fmt.Printf("repository policy: %d rulesets valid\n", len(documents))
		return
	}

	token := os.Getenv("GH_TOKEN")
	repository := os.Getenv("GITHUB_REPOSITORY")
	if token == "" || repository == "" {
		fmt.Fprintln(os.Stderr, "apply requires GH_TOKEN and GITHUB_REPOSITORY")
		os.Exit(1)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if err := apply(client, "https://api.github.com", repository, token, documents); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadPolicies(directory string) ([]policyDocument, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	files, err := fs.Glob(root.FS(), "*.json")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, errors.New("no repository rulesets found")
	}

	documents := make([]policyDocument, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		data, err := root.ReadFile(file)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var policy ruleset
		if err := decoder.Decode(&policy); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(directory, file), err)
		}
		if err := ensureEOF(decoder); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(directory, file), err)
		}
		if err := validate(policy); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(directory, file), err)
		}
		key := policy.Target + "\x00" + policy.Name
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%s: duplicate ruleset name and target", filepath.Join(directory, file))
		}
		seen[key] = struct{}{}
		documents = append(documents, policyDocument{policy: policy, payload: data})
	}
	return documents, nil
}

func validate(policy ruleset) error {
	if strings.TrimSpace(policy.Name) == "" {
		return errors.New("name is required")
	}
	if policy.Target != "branch" && policy.Target != "tag" {
		return errors.New("target must be branch or tag")
	}
	if policy.Enforcement != "active" && policy.Enforcement != "evaluate" && policy.Enforcement != "disabled" {
		return errors.New("invalid enforcement")
	}
	if !nonemptyJSONObject(policy.Conditions) {
		return errors.New("conditions must be a non-empty object")
	}
	if !nonemptyJSONArray(policy.Rules) {
		return errors.New("rules must be a non-empty array")
	}
	return nil
}

func nonemptyJSONObject(value json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && len(object) > 0
}

func nonemptyJSONArray(value json.RawMessage) bool {
	var array []json.RawMessage
	return json.Unmarshal(value, &array) == nil && len(array) > 0
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON content")
	}
	return err
}

func apply(client *http.Client, baseURL, repository, token string, documents []policyDocument) error {
	endpoint := strings.TrimRight(baseURL, "/") + "/repos/" + repository + "/rulesets"
	body, err := request(client, http.MethodGet, endpoint+"?per_page=100", token, nil)
	if err != nil {
		return fmt.Errorf("list repository rulesets: %w", err)
	}
	var remote []remoteRuleset
	if err := json.Unmarshal(body, &remote); err != nil {
		return fmt.Errorf("decode repository rulesets: %w", err)
	}

	for _, document := range documents {
		policy := document.policy
		method := http.MethodPost
		policyEndpoint := endpoint
		for _, existing := range remote {
			if existing.Name == policy.Name && existing.Target == policy.Target {
				method = http.MethodPut
				policyEndpoint = fmt.Sprintf("%s/%d", endpoint, existing.ID)
				break
			}
		}
		if _, err := request(client, method, policyEndpoint, token, document.payload); err != nil {
			return fmt.Errorf("apply %s: %w", policy.Name, err)
		}
		fmt.Printf("repository policy: %s applied\n", policy.Name)
	}
	return nil
}

func request(client *http.Client, method, endpoint, token string, body []byte) ([]byte, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	// #nosec G704 -- validateEndpoint restricts production to api.github.com and tests to loopback.
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// #nosec G704 -- validateEndpoint rejects non-GitHub and non-loopback destinations.
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid API endpoint: %w", err)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return errors.New("invalid API endpoint")
	}
	if parsed.Scheme == "https" && parsed.Hostname() == "api.github.com" && parsed.Port() == "" {
		return nil
	}
	if parsed.Scheme == "http" {
		ip := net.ParseIP(parsed.Hostname())
		if ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("API endpoint must be api.github.com over HTTPS or a loopback test server")
}
