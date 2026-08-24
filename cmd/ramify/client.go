// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// apiClient talks to ramifyd's control API, either over a unix socket or, if
// addr/token are set, a remote TCP+bearer-token listener.
type apiClient struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

func newUnixClient(socketPath string) *apiClient {
	return &apiClient{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
		baseURL: "http://unix",
	}
}

func newTCPClient(addr, token string) *apiClient {
	return &apiClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "http://" + addr,
		token:      token,
	}
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("api: unexpected status %d: %s", e.Status, e.Body)
}

// do issues an HTTP request against the API, decoding a JSON response body into
// out if out is non-nil and the response isn't empty. It returns the raw response
// for handlers that need the body verbatim (e.g. logs).
func (c *apiClient) do(ctx context.Context, method, path string, body, out any) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("client: encoding request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("client: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: %s %s: %w", method, path, err)
	}

	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }() // response is fully consumed here, nothing left for the caller
		respBody, _ := io.ReadAll(resp.Body)
		return nil, &apiError{Status: resp.StatusCode, Body: string(respBody)}
	}

	if out != nil {
		defer func() { _ = resp.Body.Close() }() // response is fully consumed here, nothing left for the caller
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("client: decoding response: %w", err)
		}
	}
	return resp, nil
}

// environment mirrors store.Environment's JSON shape without importing
// internal/store from the CLI binary.
type environment struct {
	ID           string  `json:"ID"`
	Project      string  `json:"Project"`
	Branch       string  `json:"Branch"`
	PRNumber     int     `json:"PRNumber"`
	Subdomain    string  `json:"Subdomain"`
	ArtifactRef  string  `json:"ArtifactRef"`
	DeployRef    string  `json:"DeployRef"`
	Status       string  `json:"Status"`
	Pinned       bool    `json:"Pinned"`
	TTLExpiresAt *string `json:"TTLExpiresAt"`
	CreatedAt    string  `json:"CreatedAt"`
	UpdatedAt    string  `json:"UpdatedAt"`
}

func (c *apiClient) listEnvironments(ctx context.Context, project, branch string) ([]environment, error) {
	path := "/environments/"
	if project != "" || branch != "" {
		q := make(url.Values)
		if project != "" {
			q.Set("project", project)
		}
		if branch != "" {
			q.Set("branch", branch)
		}
		path += "?" + q.Encode()
	}
	var envs []environment
	if _, err := c.do(ctx, http.MethodGet, path, nil, &envs); err != nil {
		return nil, err
	}
	return envs, nil
}

func (c *apiClient) getEnvironment(ctx context.Context, id string) (environment, error) {
	var env environment
	if _, err := c.do(ctx, http.MethodGet, "/environments/"+id+"/", nil, &env); err != nil {
		return environment{}, err
	}
	return env, nil
}

func (c *apiClient) deleteEnvironment(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/environments/"+id+"/", nil, nil)
	return err
}

func (c *apiClient) logs(ctx context.Context, id string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/environments/"+id+"/logs", nil, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }() // fully read below; nothing left to do on close error
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("client: reading logs: %w", err)
	}
	return string(body), nil
}

func (c *apiClient) healthz(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/healthz", nil, nil)
	return err
}
