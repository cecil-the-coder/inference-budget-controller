/*
Copyright 2024 eh-ops.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package xet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultHuggingFaceHost is the default HuggingFace Hub host.
	DefaultHuggingFaceHost = "huggingface.co"
	// DefaultXetAPIEndpoint is the default Xet API endpoint.
	DefaultXetAPIEndpoint = "https://xetfs.huggingface.co"
	// DefaultAuthEndpoint is the default Xet authentication endpoint.
	DefaultAuthEndpoint = "https://huggingface.co/api/xet-auth"

	// DefaultTimeout is the default HTTP request timeout.
	DefaultTimeout = 30 * time.Minute
)

// ReconstructionResponse represents the response from the reconstruction API.
type ReconstructionResponse struct {
	// OffsetIntoFirstRange is the byte offset into the first range.
	OffsetIntoFirstRange int64 `json:"offset_into_first_range"`

	// Terms contains the chunk metadata for reconstruction.
	Terms []ReconstructionTerm `json:"terms"`

	// FetchInfo contains the URLs and ranges for fetching xorb data.
	FetchInfo map[string][]FetchInfo `json:"fetch_info"`
}

// ReconstructionTerm represents a single term in the reconstruction.
type ReconstructionTerm struct {
	// Hash is the content hash of the xorb.
	Hash string `json:"hash"`

	// UnpackedLength is the unpacked size in bytes.
	UnpackedLength int64 `json:"unpacked_length"`

	// Range specifies which chunk range to use from the xorb.
	Range ByteRange `json:"range"`
}

// FetchInfo contains URL and range information for fetching a xorb.
type FetchInfo struct {
	// URL is the presigned URL to fetch the xorb from.
	URL string `json:"url"`

	// URLRange is the byte range within the fetched URL.
	URLRange ByteRange `json:"url_range"`

	// Range is the range within the xorb.
	Range ByteRange `json:"range"`
}

// ByteRange represents a range of bytes.
type ByteRange struct {
	// Start is the start byte offset.
	Start int64 `json:"start"`

	// End is the end byte offset (exclusive).
	End int64 `json:"end"`
}

// AuthResponse represents the response from the Xet authentication endpoint.
type AuthResponse struct {
	// Token is the authentication token for Xet API.
	Token string `json:"token"`

	// ExpiresAt is when the token expires.
	ExpiresAt time.Time `json:"expires_at"`
}

// Client is the Xet API client.
type Client struct {
	httpClient     *http.Client
	hfHost         string
	xetAPIEndpoint string
	authEndpoint   string
	token          string
	tokenMutex     sync.RWMutex
	cachedAuth     *AuthResponse
	authMutex      sync.Mutex
}

// ClientOption is a functional option for configuring the Client.
type ClientOption func(*Client)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// WithHuggingFaceHost sets a custom HuggingFace host.
func WithHuggingFaceHost(host string) ClientOption {
	return func(c *Client) {
		c.hfHost = host
	}
}

// WithXetAPIEndpoint sets a custom Xet API endpoint.
func WithXetAPIEndpoint(endpoint string) ClientOption {
	return func(c *Client) {
		c.xetAPIEndpoint = endpoint
	}
}

// WithAuthEndpoint sets a custom authentication endpoint.
func WithAuthEndpoint(endpoint string) ClientOption {
	return func(c *Client) {
		c.authEndpoint = endpoint
	}
}

// WithToken sets the authentication token directly.
func WithToken(token string) ClientOption {
	return func(c *Client) {
		c.token = token
	}
}

// NewClient creates a new Xet API client.
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		hfHost:         DefaultHuggingFaceHost,
		xetAPIEndpoint: DefaultXetAPIEndpoint,
		authEndpoint:   DefaultAuthEndpoint,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// SetToken sets the authentication token.
func (c *Client) SetToken(token string) {
	c.tokenMutex.Lock()
	defer c.tokenMutex.Unlock()
	c.token = token
}

// getToken returns the current authentication token.
func (c *Client) getToken() string {
	c.tokenMutex.RLock()
	defer c.tokenMutex.RUnlock()
	return c.token
}

// getAuthToken gets a valid authentication token, refreshing if necessary.
func (c *Client) getAuthToken(ctx context.Context) (string, error) {
	// If a token is set directly, use it
	if token := c.getToken(); token != "" {
		return token, nil
	}

	// Check if we have a cached auth token that's still valid
	c.authMutex.Lock()
	defer c.authMutex.Unlock()

	if c.cachedAuth != nil && time.Now().Add(5*time.Minute).Before(c.cachedAuth.ExpiresAt) {
		return c.cachedAuth.Token, nil
	}

	// Fetch a new token
	auth, err := c.fetchAuthToken(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to fetch auth token: %w", err)
	}

	c.cachedAuth = auth
	return auth.Token, nil
}

// fetchAuthToken fetches a new authentication token from the auth endpoint.
func (c *Client) fetchAuthToken(ctx context.Context) (*AuthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.authEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create auth request: %w", err)
	}

	// Add HF token if available
	if token := c.getToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("auth request returned status %d: %s", resp.StatusCode, string(body))
	}

	var authResp AuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return nil, fmt.Errorf("failed to decode auth response: %w", err)
	}

	return &authResp, nil
}

// ResolveFileID resolves the Xet file ID from a HuggingFace Hub file URL.
// The file ID is returned in the X-Xet-Hash response header.
func (c *Client) ResolveFileID(ctx context.Context, namespace, repo, branch, filepath string) (string, error) {
	resolveURL := fmt.Sprintf("https://%s/%s/%s/resolve/%s/%s",
		c.hfHost, namespace, repo, branch, filepath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolveURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create resolve request: %w", err)
	}

	// Add authentication
	token, err := c.getAuthToken(ctx)
	if err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// Don't follow redirects, we just want the headers
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: c.httpClient.Timeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check for redirect and get the X-Xet-Hash header
	fileID := resp.Header.Get("X-Xet-Hash")
	if fileID == "" {
		// Try following redirects
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			if location != "" {
				// Follow the redirect to get the actual file ID
				return c.resolveFileIDFromURL(ctx, location)
			}
		}
		return "", fmt.Errorf("X-Xet-Hash header not found in response")
	}

	return fileID, nil
}

// resolveFileIDFromURL follows a redirect URL to get the file ID.
func (c *Client) resolveFileIDFromURL(ctx context.Context, location string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, location, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	token, err := c.getAuthToken(ctx)
	if err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	fileID := resp.Header.Get("X-Xet-Hash")
	if fileID == "" {
		return "", fmt.Errorf("X-Xet-Hash header not found in response")
	}

	return fileID, nil
}

// GetReconstruction fetches the reconstruction metadata for a file ID.
func (c *Client) GetReconstruction(ctx context.Context, fileID string) (*ReconstructionResponse, error) {
	reconURL := fmt.Sprintf("%s/v1/reconstructions/%s", c.xetAPIEndpoint, fileID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reconURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create reconstruction request: %w", err)
	}

	token, err := c.getAuthToken(ctx)
	if err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reconstruction request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("reconstruction request returned status %d: %s", resp.StatusCode, string(body))
	}

	var reconResp ReconstructionResponse
	if err := json.NewDecoder(resp.Body).Decode(&reconResp); err != nil {
		return nil, fmt.Errorf("failed to decode reconstruction response: %w", err)
	}

	return &reconResp, nil
}

// FetchXorbRange fetches a byte range from a xorb URL.
func (c *Client) FetchXorbRange(ctx context.Context, xorbURL string, start, end int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xorbURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create fetch request: %w", err)
	}

	// Set range header
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch request returned status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return data, nil
}

// FetchXorb fetches an entire xorb from a URL.
func (c *Client) FetchXorb(ctx context.Context, xorbURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xorbURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create fetch request: %w", err)
	}

	// Check if URL has query parameters (presigned URLs often do)
	if strings.Contains(xorbURL, "?") {
		// Validate the URL
		parsed, err := url.Parse(xorbURL)
		if err != nil {
			return nil, fmt.Errorf("invalid xorb URL: %w", err)
		}
		_ = parsed // URL is valid
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch request returned status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return data, nil
}

// FetchXorbFromFetchInfo fetches xorb data using FetchInfo.
func (c *Client) FetchXorbFromFetchInfo(ctx context.Context, info FetchInfo) ([]byte, error) {
	return c.FetchXorbRange(ctx, info.URL, info.URLRange.Start, info.URLRange.End)
}
