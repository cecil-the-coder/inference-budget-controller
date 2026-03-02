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

// DownloadSpec specifies what files to download from a repository.
type DownloadSpec struct {
	// Repo is the repository identifier (e.g., "organization/model-name").
	Repo string

	// Branch is the git branch to download from.
	// Defaults to "main" if not specified.
	Branch string

	// Files is a list of exact filenames to download.
	// These are matched exactly against file paths in the repository.
	Files []string

	// Patterns is a list of glob patterns to match files.
	// Standard filepath.Match patterns are supported (e.g., "*.json", "model*.bin").
	Patterns []string

	// Exclude is a list of patterns for files to exclude from download.
	// Files matching any exclude pattern will be skipped.
	Exclude []string

	// DestDir is the destination directory for downloaded files.
	// If empty, the default cache directory is used.
	DestDir string
}

// DownloadOption is a functional option for configuring a DownloadSpec.
type DownloadOption func(*DownloadSpec)

// WithBranch sets the branch for the download.
func WithBranch(branch string) DownloadOption {
	return func(s *DownloadSpec) {
		s.Branch = branch
	}
}

// WithFiles sets the exact files to download.
func WithFiles(files ...string) DownloadOption {
	return func(s *DownloadSpec) {
		s.Files = files
	}
}

// WithPatterns sets the glob patterns for files to download.
func WithPatterns(patterns ...string) DownloadOption {
	return func(s *DownloadSpec) {
		s.Patterns = patterns
	}
}

// WithExclude sets patterns for files to exclude.
func WithExclude(exclude ...string) DownloadOption {
	return func(s *DownloadSpec) {
		s.Exclude = exclude
	}
}

// WithDestDir sets the destination directory.
func WithDestDir(dir string) DownloadOption {
	return func(s *DownloadSpec) {
		s.DestDir = dir
	}
}

// NewDownloadSpec creates a new DownloadSpec with the given repository and options.
func NewDownloadSpec(repo string, opts ...DownloadOption) *DownloadSpec {
	spec := &DownloadSpec{
		Repo:   repo,
		Branch: "main",
	}

	for _, opt := range opts {
		opt(spec)
	}

	return spec
}
