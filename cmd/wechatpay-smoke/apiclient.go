package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// apiResponse is one captured HTTP response from the local application server.
type apiResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *apiResponse) decode(v any) error {
	if r == nil {
		return fmt.Errorf("no response")
	}
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decode response (status %d, body %q): %w", r.Status, truncateForError(r.Body), err)
	}
	return nil
}

func (r *apiResponse) text() string { return string(r.Body) }

func (r *apiResponse) success() bool { return r != nil && r.Status >= 200 && r.Status < 300 }

// httpClient talks to the loopback application server with the credentials the
// smoke tenant's API key carries.
type httpClient struct {
	baseURL string
	apiKey  string
	envID   string
	client  *http.Client
}

func newHTTPClient(baseURL, apiKey, envID string) *httpClient {
	return &httpClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		envID:   envID,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// private sends an authenticated request on the private API surface.
func (c *httpClient) private(ctx context.Context, method, path string, body any) (*apiResponse, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("X-Environment-ID", c.envID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req)
}

// raw sends exact bytes with exact headers; used for signed provider callbacks,
// where any re-encoding would invalidate the signature.
func (c *httpClient) raw(ctx context.Context, method, path string, body []byte, headers http.Header) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	return c.do(req)
}

func (c *httpClient) do(req *http.Request) (*apiResponse, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &apiResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}, nil
}

func truncateForError(body []byte) string {
	const limit = 512
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "…"
}
