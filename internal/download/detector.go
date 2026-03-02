package download

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// StorageType represents the type of storage backend for a file.
type StorageType string

const (
	// StorageTypeUnknown indicates the storage type could not be determined.
	StorageTypeUnknown StorageType = "Unknown"
	// StorageTypeXet indicates the file is stored using Xet storage.
	StorageTypeXet StorageType = "Xet"
	// StorageTypeLFS indicates the file is stored using standard Git LFS.
	StorageTypeLFS StorageType = "LFS"
)

// DetectionResult contains the result of a storage type detection.
type DetectionResult struct {
	// Type indicates whether the file is stored on Xet or LFS.
	Type StorageType
	// FileID is only set for Xet storage and contains the Xet hash.
	FileID string
}

// cachedResult wraps a DetectionResult with its cache timestamp.
type cachedResult struct {
	result    DetectionResult
	timestamp time.Time
}

// Detector detects whether files are stored on Xet or standard LFS.
type Detector struct {
	client   *http.Client
	token    string
	cache    map[string]cachedResult
	cacheMu  sync.RWMutex
	cacheTTL time.Duration
}

// DetectorOption configures a Detector.
type DetectorOption func(*Detector)

// WithDetectorHTTPClient sets a custom HTTP client for the detector.
func WithDetectorHTTPClient(c *http.Client) DetectorOption {
	return func(d *Detector) {
		d.client = c
	}
}

// WithDetectorToken sets the authentication token for Hugging Face API requests.
func WithDetectorToken(token string) DetectorOption {
	return func(d *Detector) {
		d.token = token
	}
}

// WithDetectorCacheTTL sets the cache time-to-live duration.
func WithDetectorCacheTTL(ttl time.Duration) DetectorOption {
	return func(d *Detector) {
		d.cacheTTL = ttl
	}
}

// NewDetector creates a new Detector with the given options.
func NewDetector(opts ...DetectorOption) *Detector {
	d := &Detector{
		client:   http.DefaultClient,
		cache:    make(map[string]cachedResult),
		cacheTTL: 5 * time.Minute,
	}

	for _, opt := range opts {
		opt(d)
	}

	return d
}

// Detect checks if a file is stored on Xet or LFS by making a HEAD request
// to the Hugging Face resolve URL and checking for the X-Xet-Hash header.
func (d *Detector) Detect(ctx context.Context, repo, branch, file string) (DetectionResult, error) {
	// 1. Check cache first
	cacheKey := fmt.Sprintf("%s/%s/%s", repo, branch, file)
	if cached, ok := d.getCached(cacheKey); ok {
		return cached, nil
	}

	// 2. Make HEAD request to resolve URL
	url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", repo, branch, file)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return DetectionResult{Type: StorageTypeUnknown}, fmt.Errorf("failed to create request: %w", err)
	}

	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}

	// Don't follow redirects - we want the headers from the redirect response
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	client = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: client.Timeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return DetectionResult{Type: StorageTypeUnknown}, fmt.Errorf("failed to make HEAD request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 3. Check for X-Xet-Hash header - indicates Xet storage
	if xetHash := resp.Header.Get("X-Xet-Hash"); xetHash != "" {
		result := DetectionResult{
			Type:   StorageTypeXet,
			FileID: xetHash,
		}
		d.setCached(cacheKey, result)
		return result, nil
	}

	// 4. Standard LFS
	result := DetectionResult{Type: StorageTypeLFS}
	d.setCached(cacheKey, result)
	return result, nil
}

// DetectMultiple checks multiple files and returns results for each.
// Errors are reported per-file in the returned error map; the returned error
// is nil if all detections succeed.
func (d *Detector) DetectMultiple(ctx context.Context, repo, branch string, files []string) (map[string]DetectionResult, map[string]error) {
	results := make(map[string]DetectionResult, len(files))
	errors := make(map[string]error)

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, file := range files {
		wg.Add(1)
		go func(filename string) {
			defer wg.Done()

			result, err := d.Detect(ctx, repo, branch, filename)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errors[filename] = err
				results[filename] = DetectionResult{Type: StorageTypeUnknown}
			} else {
				results[filename] = result
			}
		}(file)
	}

	wg.Wait()
	return results, errors
}

// getCached retrieves a cached result if it exists and hasn't expired.
func (d *Detector) getCached(key string) (DetectionResult, bool) {
	d.cacheMu.RLock()
	defer d.cacheMu.RUnlock()

	cached, ok := d.cache[key]
	if !ok {
		return DetectionResult{}, false
	}

	// Check if the cache entry has expired
	if time.Since(cached.timestamp) > d.cacheTTL {
		return DetectionResult{}, false
	}

	return cached.result, true
}

// setCached stores a result in the cache with the current timestamp.
func (d *Detector) setCached(key string, result DetectionResult) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()

	d.cache[key] = cachedResult{
		result:    result,
		timestamp: time.Now(),
	}
}
