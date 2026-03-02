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

// Package huggingface provides a wrapper around the go-huggingface library
// for interacting with the HuggingFace Hub from the inference-budget-controller.
package huggingface

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gomlx/go-huggingface/hub"
)

const (
	// DefaultTokenEnvVar is the default environment variable name for the HuggingFace token.
	DefaultTokenEnvVar = "HF_TOKEN"
)

// RepositoryType represents the type of HuggingFace repository.
type RepositoryType string

const (
	// RepositoryTypeModel indicates a model repository.
	RepositoryTypeModel RepositoryType = "model"
	// RepositoryTypeDataset indicates a dataset repository.
	RepositoryTypeDataset RepositoryType = "dataset"
	// RepositoryTypeSpace indicates a space repository.
	RepositoryTypeSpace RepositoryType = "space"
)

// Config holds configuration for the HuggingFace client.
type Config struct {
	// Token is the HuggingFace API token for authentication.
	// If empty, the client will look for the token in the environment.
	Token string

	// TokenEnvVar is the environment variable name to read the token from.
	// Defaults to DefaultTokenEnvVar ("HF_TOKEN") if not specified.
	TokenEnvVar string

	// CacheDir is the directory to cache downloaded files.
	// If empty, uses the default cache directory.
	CacheDir string
}

// Client provides methods to interact with the HuggingFace Hub.
type Client struct {
	config Config
	mu     sync.RWMutex
}

// NewClient creates a new HuggingFace client with the given configuration.
func NewClient(config Config) *Client {
	if config.TokenEnvVar == "" {
		config.TokenEnvVar = DefaultTokenEnvVar
	}
	return &Client{
		config: config,
	}
}

// getToken returns the authentication token from config or environment.
func (c *Client) getToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// First check if token is directly configured
	if c.config.Token != "" {
		return c.config.Token
	}

	// Fall back to environment variable
	return os.Getenv(c.config.TokenEnvVar)
}

// SetToken allows updating the authentication token at runtime.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.config.Token = token
}

// Repository represents a HuggingFace repository.
type Repository struct {
	client *Client
	repo   *hub.Repo
	id     string
}

// NewRepository creates a new repository handle for the given repository ID.
// The repository ID is typically in the format "organization/model-name".
func (c *Client) NewRepository(ctx context.Context, repoID string, repoType RepositoryType) (*Repository, error) {
	token := c.getToken()

	repo := hub.New(repoID).WithAuth(token)

	switch repoType {
	case RepositoryTypeModel:
		repo = repo.WithType(hub.RepoTypeModel)
	case RepositoryTypeDataset:
		repo = repo.WithType(hub.RepoTypeDataset)
	case RepositoryTypeSpace:
		repo = repo.WithType(hub.RepoTypeSpace)
	default:
		return nil, fmt.Errorf("unsupported repository type: %s", repoType)
	}

	// Set cache directory if configured
	if c.config.CacheDir != "" {
		repo = repo.WithCacheDir(c.config.CacheDir)
	}

	return &Repository{
		client: c,
		repo:   repo,
		id:     repoID,
	}, nil
}

// NewModelRepository is a convenience method to create a model repository handle.
func (c *Client) NewModelRepository(ctx context.Context, modelID string) (*Repository, error) {
	return c.NewRepository(ctx, modelID, RepositoryTypeModel)
}

// NewDatasetRepository is a convenience method to create a dataset repository handle.
func (c *Client) NewDatasetRepository(ctx context.Context, datasetID string) (*Repository, error) {
	return c.NewRepository(ctx, datasetID, RepositoryTypeDataset)
}

// ID returns the repository identifier.
func (r *Repository) ID() string {
	return r.id
}

// FileInfo contains information about a file in a repository.
type FileInfo struct {
	// Name is the file name/path within the repository.
	Name string

	// Size is the file size in bytes (if available).
	Size int64
}

// ListFiles returns a list of files in the repository.
func (r *Repository) ListFiles(ctx context.Context) ([]FileInfo, error) {
	var files []FileInfo

	for fileName, err := range r.repo.IterFileNames() {
		if err != nil {
			return nil, fmt.Errorf("error iterating files: %w", err)
		}
		files = append(files, FileInfo{
			Name: fileName,
		})
	}

	return files, nil
}

// FileExists checks if a file exists in the repository.
func (r *Repository) FileExists(ctx context.Context, filePath string) (bool, error) {
	files, err := r.ListFiles(ctx)
	if err != nil {
		return false, err
	}

	for _, f := range files {
		if f.Name == filePath {
			return true, nil
		}
	}
	return false, nil
}

// DownloadFile downloads a file from the repository and returns the local file path.
func (r *Repository) DownloadFile(ctx context.Context, filePath string) (string, error) {
	localPath, err := r.repo.DownloadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to download file %s: %w", filePath, err)
	}
	return localPath, nil
}

// DownloadFiles downloads multiple files from the repository and returns a map of
// repository paths to local file paths.
func (r *Repository) DownloadFiles(ctx context.Context, filePaths []string) (map[string]string, error) {
	results := make(map[string]string)
	for _, filePath := range filePaths {
		localPath, err := r.DownloadFile(ctx, filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to download %s: %w", filePath, err)
		}
		results[filePath] = localPath
	}
	return results, nil
}

// ResolveModelPath resolves the local path for a model file, downloading it if necessary.
// This is useful for getting the path to model weights, config files, etc.
func (r *Repository) ResolveModelPath(ctx context.Context, fileName string) (string, error) {
	return r.DownloadFile(ctx, fileName)
}

// DownloadModel downloads all files matching the given patterns from the repository.
// Patterns are standard filepath.Match patterns (e.g., "*.json", "pytorch_model*.bin").
func (r *Repository) DownloadModel(ctx context.Context, patterns []string) (map[string]string, error) {
	files, err := r.ListFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	var toDownload []string
	for _, file := range files {
		for _, pattern := range patterns {
			matched, err := filepath.Match(pattern, file.Name)
			if err != nil {
				return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
			}
			if matched {
				toDownload = append(toDownload, file.Name)
				break
			}
		}
	}

	if len(toDownload) == 0 {
		return nil, fmt.Errorf("no files found matching patterns: %v", patterns)
	}

	return r.DownloadFiles(ctx, toDownload)
}
