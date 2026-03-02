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
	"fmt"
	"strings"
)

// FileIDResolver resolves Xet file IDs from HuggingFace Hub URLs.
type FileIDResolver struct {
	client *Client
}

// NewFileIDResolver creates a new file ID resolver.
func NewFileIDResolver(client *Client) *FileIDResolver {
	return &FileIDResolver{client: client}
}

// HuggingFaceURL represents a parsed HuggingFace Hub URL.
type HuggingFaceURL struct {
	// Namespace is the organization or user name.
	Namespace string
	// Repo is the repository name.
	Repo string
	// Branch is the git branch (default: "main").
	Branch string
	// FilePath is the path to the file within the repository.
	FilePath string
}

// ParseHuggingFaceURL parses a HuggingFace Hub URL.
// Supported formats:
// - huggingface.co/{namespace}/{repo}/resolve/{branch}/{filepath}
// - huggingface.co/{namespace}/{repo}/blob/{branch}/{filepath}
// - https://huggingface.co/{namespace}/{repo}/resolve/{branch}/{filepath}
// - hf://{namespace}/{repo}/{filepath} (defaults to main branch)
func ParseHuggingFaceURL(url string) (*HuggingFaceURL, error) {
	// Handle hf:// protocol
	if strings.HasPrefix(url, "hf://") {
		return parseHFProtocol(url)
	}

	// Remove protocol prefix if present
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")

	// Remove host prefix
	url = strings.TrimPrefix(url, "huggingface.co/")
	url = strings.TrimPrefix(url, "hf.co/")

	parts := strings.SplitN(url, "/", 4)
	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid HuggingFace URL format: %s", url)
	}

	namespace := parts[0]
	repo := parts[1]
	branch := "main"
	filePath := ""

	// Handle both /resolve/{branch}/... and /blob/{branch}/... formats
	if parts[2] == "resolve" || parts[2] == "blob" {
		if len(parts) < 4 {
			return nil, fmt.Errorf("invalid HuggingFace URL format: %s", url)
		}
		branch = parts[2]
		// Get branch and filepath
		remaining := parts[3]
		subParts := strings.SplitN(remaining, "/", 2)
		if len(subParts) >= 1 {
			branch = subParts[0]
		}
		if len(subParts) >= 2 {
			filePath = subParts[1]
		}
	} else {
		// Assume format is {namespace}/{repo}/{branch}/{filepath}
		branch = parts[2]
		if len(parts) >= 4 {
			filePath = parts[3]
		}
	}

	return &HuggingFaceURL{
		Namespace: namespace,
		Repo:      repo,
		Branch:    branch,
		FilePath:  filePath,
	}, nil
}

// parseHFProtocol parses hf:// protocol URLs.
func parseHFProtocol(url string) (*HuggingFaceURL, error) {
	// Remove hf:// prefix
	url = strings.TrimPrefix(url, "hf://")

	parts := strings.SplitN(url, "/", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid hf:// URL format: %s", url)
	}

	namespace := parts[0]
	repo := parts[1]
	filePath := ""
	branch := "main"

	if len(parts) >= 3 {
		remaining := parts[2]
		// Check if the first segment looks like a branch name
		subParts := strings.SplitN(remaining, "/", 2)
		if len(subParts) == 1 {
			filePath = remaining
		} else {
			// First segment could be a branch or part of the path
			// If it's a known branch name, use it; otherwise treat whole thing as path
			potentialBranch := subParts[0]
			if potentialBranch == "main" || potentialBranch == "master" {
				branch = potentialBranch
				filePath = subParts[1]
			} else {
				filePath = remaining
			}
		}
	}

	return &HuggingFaceURL{
		Namespace: namespace,
		Repo:      repo,
		Branch:    branch,
		FilePath:  filePath,
	}, nil
}

// Resolve resolves the Xet file ID from a HuggingFace URL.
func (r *FileIDResolver) Resolve(ctx context.Context, hfURL *HuggingFaceURL) (string, error) {
	return r.client.ResolveFileID(ctx, hfURL.Namespace, hfURL.Repo, hfURL.Branch, hfURL.FilePath)
}

// ResolveFromURL parses a URL string and resolves the file ID.
func (r *FileIDResolver) ResolveFromURL(ctx context.Context, url string) (string, error) {
	hfURL, err := ParseHuggingFaceURL(url)
	if err != nil {
		return "", err
	}
	return r.Resolve(ctx, hfURL)
}

// ResolveMultiple resolves file IDs for multiple files in a repository.
func (r *FileIDResolver) ResolveMultiple(ctx context.Context, namespace, repo, branch string, filePaths []string) (map[string]string, error) {
	results := make(map[string]string)
	for _, filePath := range filePaths {
		fileID, err := r.client.ResolveFileID(ctx, namespace, repo, branch, filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve %s: %w", filePath, err)
		}
		results[filePath] = fileID
	}
	return results, nil
}

// String returns the full URL for the HuggingFace file.
func (u *HuggingFaceURL) String() string {
	return fmt.Sprintf("https://huggingface.co/%s/%s/resolve/%s/%s",
		u.Namespace, u.Repo, u.Branch, u.FilePath)
}

// RepositoryID returns the repository identifier (namespace/repo).
func (u *HuggingFaceURL) RepositoryID() string {
	return fmt.Sprintf("%s/%s", u.Namespace, u.Repo)
}
