// client.go
// Minimal HTTP client helpers for RaftCard API
// (Library-style; import into your tests or a small CLI.)
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type CreateApplicationRequest struct {
	IdempotencyKey string      `json:"idempotency_key"`
	Applicant      interface{} `json:"applicant"`
}

type CreateApplicationResponse struct {
	ApplicationID string `json:"application_id"`
	Status        string `json:"status"`
}

type StatusResponse struct {
	ApplicationID string   `json:"application_id"`
	Status        string   `json:"status"`
	Reasons       []string `json:"reasons"`
}

// CreateApplication posts to /applications 
func CreateApplication(baseURL string, req CreateApplicationRequest) (CreateApplicationResponse, error) {
	var out CreateApplicationResponse
	body, _ := json.Marshal(req)
	url := strings.TrimRight(baseURL, "/") + "/applications"
	do := func(u string) (*http.Response, error) {
		return http.Post(u, "application/json", bytes.NewReader(body))
	}
	resp, err := do(url)
	if err != nil { return out, err }
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTemporaryRedirect {
		leader := resp.Header.Get("Leader-Location")
		if leader == "" { return out, errors.New("redirected but missing Leader-Location") }
		resp2, err := do(strings.TrimRight(leader, "/") + "/applications")
		if err != nil { return out, err }
		defer resp2.Body.Close()
		return decodeCreate(resp2)
	}
	return decodeCreate(resp)
}

func decodeCreate(resp *http.Response) (CreateApplicationResponse, error) {
	var out CreateApplicationResponse
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 { return out, fmt.Errorf("%s: %s", resp.Status, string(b)) }
	if err := json.Unmarshal(b, &out); err != nil { return out, err }
	return out, nil
}

// GetStatus fetches /applications/{id}. Follows a single leader redirect.
func GetStatus(baseURL, appID string) (StatusResponse, error) {
	var out StatusResponse
	url := strings.TrimRight(baseURL, "/") + "/applications/" + appID
	do := func(u string) (*http.Response, error) { return http.Get(u) }
	resp, err := do(url)
	if err != nil { return out, err }
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTemporaryRedirect {
		leader := resp.Header.Get("Leader-Location")
		if leader == "" { return out, errors.New("redirected but missing Leader-Location") }
		resp2, err := do(strings.TrimRight(leader, "/") + "/applications/" + appID)
		if err != nil { return out, err }
		defer resp2.Body.Close()
		return decodeStatus(resp2)
	}
	return decodeStatus(resp)
}

func decodeStatus(resp *http.Response) (StatusResponse, error) {
	var out StatusResponse
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 { return out, fmt.Errorf("%s: %s", resp.Status, string(b)) }
	if err := json.Unmarshal(b, &out); err != nil { return out, err }
	return out, nil
}
