package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

type registryPublishFinding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Kind    string `json:"kind"`
	Excerpt string `json:"excerpt"`
}

type registryPublishError struct {
	Error      string                   `json:"error"`
	Findings   []registryPublishFinding `json:"findings"`
	Violations []string                 `json:"violations"`
}

func publishToRegistry(registryURL, token, owner, name, bundlePath string) (status int, body []byte, err error) {
	endpoint, err := registryEndpoint(registryURL, "v1", "stacks", owner, name, "versions")
	if err != nil {
		return 0, nil, err
	}
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		return 0, nil, fmt.Errorf("read bundle: %w", err)
	}

	var reqBody bytes.Buffer
	writer := multipart.NewWriter(&reqBody)
	part, err := writer.CreateFormFile("bundle", path.Base(bundlePath))
	if err != nil {
		return 0, nil, err
	}
	if _, err := part.Write(bundle); err != nil {
		return 0, nil, err
	}
	if err := writer.Close(); err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, &reqBody)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("registry publish: %w", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("registry publish: %w", err)
	}
	return resp.StatusCode, body, nil
}

func loadRegistrySearch(registryURL, query string) (indexDocument, error) {
	endpoint, err := registryEndpoint(registryURL, "v1", "search")
	if err != nil {
		return indexDocument{}, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return indexDocument{}, err
	}
	q := u.Query()
	q.Set("q", query)
	u.RawQuery = q.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return indexDocument{}, fmt.Errorf("registry search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return indexDocument{}, fmt.Errorf("registry search: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return indexDocument{}, fmt.Errorf("registry search: %s", resp.Status)
	}
	var doc indexDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return indexDocument{}, fmt.Errorf("registry search: invalid JSON: %w", err)
	}
	return doc, nil
}

func resolveRegistryRef(ref string) (string, error) {
	if !strings.HasPrefix(ref, "@") {
		return ref, nil
	}
	registryURL := strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_URL"))
	if registryURL == "" {
		return "", fmt.Errorf("SHERPA_REGISTRY_URL is required for registry ref %q", ref)
	}
	ownerName := strings.TrimPrefix(ref, "@")
	owner, name, ok := strings.Cut(ownerName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("registry ref must be @owner/name")
	}
	return registryEndpoint(registryURL, "v1", "stacks", owner, name+".git")
}

func registryEndpoint(base string, elems ...string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("registry URL is required")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("registry URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("registry URL must include scheme and host")
	}
	parts := []string{strings.TrimRight(u.Path, "/")}
	for _, elem := range elems {
		parts = append(parts, url.PathEscape(elem))
	}
	u.Path = path.Join(parts...)
	return u.String(), nil
}
