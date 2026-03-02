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

package download

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/cecil-the-coder/inference-budget-controller/internal/huggingface"
)

// FileResolver resolves file patterns to actual file names from a HuggingFace repository.
// It supports explicit file lists, glob patterns, and exclusion patterns.
type FileResolver struct {
	hfClient *huggingface.Client
}

// NewFileResolver creates a new FileResolver with the given HuggingFace client.
func NewFileResolver(hfClient *huggingface.Client) *FileResolver {
	return &FileResolver{
		hfClient: hfClient,
	}
}

// Resolve expands patterns and filters to get the actual list of files to download.
// It performs the following steps:
// 1. Lists all files in the repository
// 2. Starts with any explicitly specified files
// 3. Adds files matching any of the glob patterns
// 4. Removes files matching any exclusion patterns
// 5. Returns a sorted list of unique file paths
func (r *FileResolver) Resolve(ctx context.Context, repo string, spec *DownloadSpec) ([]string, error) {
	// 1. List all files in the repo
	hfRepo, err := r.hfClient.NewModelRepository(ctx, repo)
	if err != nil {
		return nil, err
	}

	allFiles, err := hfRepo.ListFiles(ctx)
	if err != nil {
		return nil, err
	}

	// 2. Start with explicit files
	files := make(map[string]bool)
	for _, f := range spec.Files {
		files[f] = true
	}

	// 3. Add files matching patterns
	for _, pattern := range spec.Patterns {
		for _, f := range allFiles {
			matched, err := filepath.Match(pattern, f.Name)
			if err != nil {
				// Invalid pattern, skip it
				continue
			}
			if matched {
				files[f.Name] = true
			}
		}
	}

	// If no files or patterns specified, include all files
	if len(spec.Files) == 0 && len(spec.Patterns) == 0 {
		for _, f := range allFiles {
			files[f.Name] = true
		}
	}

	// 4. Remove excluded files
	for _, excl := range spec.Exclude {
		for f := range files {
			matched, err := filepath.Match(excl, f)
			if err != nil {
				// Invalid pattern, skip it
				continue
			}
			if matched {
				delete(files, f)
			}
		}
	}

	// 5. Convert to sorted slice
	result := make([]string, 0, len(files))
	for f := range files {
		result = append(result, f)
	}
	sort.Strings(result)
	return result, nil
}

// FileInfo contains information about a file to download.
type FileInfo struct {
	// Path is the file path within the repository.
	Path string

	// Size is the file size in bytes (if available).
	Size int64

	// IsLFS indicates whether the file is stored in Git LFS.
	IsLFS bool
}

// ResolveWithInfo resolves files and returns detailed information about each file.
// This is useful when you need file sizes or LFS status before downloading.
func (r *FileResolver) ResolveWithInfo(ctx context.Context, repo string, spec *DownloadSpec) ([]FileInfo, error) {
	// Get the list of file paths
	paths, err := r.Resolve(ctx, repo, spec)
	if err != nil {
		return nil, err
	}

	// Get detailed file information from the repository
	hfRepo, err := r.hfClient.NewModelRepository(ctx, repo)
	if err != nil {
		return nil, err
	}

	allFiles, err := hfRepo.ListFiles(ctx)
	if err != nil {
		return nil, err
	}

	// Create a map for quick lookup
	fileMap := make(map[string]huggingface.FileInfo)
	for _, f := range allFiles {
		fileMap[f.Name] = f
	}

	// Build result with info
	result := make([]FileInfo, 0, len(paths))
	for _, path := range paths {
		info := FileInfo{
			Path: path,
		}
		if hfInfo, ok := fileMap[path]; ok {
			info.Size = hfInfo.Size
			// Heuristic: files larger than 10MB are likely LFS
			info.IsLFS = hfInfo.Size > 10*1024*1024
		}
		result = append(result, info)
	}

	return result, nil
}
