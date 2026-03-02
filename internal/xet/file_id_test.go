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
	"testing"
)

func TestParseHuggingFaceURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantErr  bool
		expected *HuggingFaceURL
	}{
		// Standard huggingface.co URLs with /resolve/
		{
			name:    "standard resolve URL",
			url:     "https://huggingface.co/bert-base-uncased/resolve/main/config.json",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "bert-base-uncased",
				Repo:      "resolve",
				Branch:    "main",
				FilePath:  "config.json",
			},
		},
		{
			name:    "resolve URL with nested path",
			url:     "https://huggingface.co/org/repo/resolve/main/models/config.json",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "models/config.json",
			},
		},
		{
			name:    "resolve URL without https",
			url:     "huggingface.co/org/repo/resolve/main/file.txt",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "file.txt",
			},
		},
		{
			name:    "resolve URL with http",
			url:     "http://huggingface.co/org/repo/resolve/main/file.txt",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "file.txt",
			},
		},
		// hf:// protocol URLs
		{
			name:    "hf:// simple",
			url:     "hf://org/repo/file.txt",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "file.txt",
			},
		},
		{
			name:    "hf:// with main branch",
			url:     "hf://org/repo/main/file.txt",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "file.txt",
			},
		},
		{
			name:    "hf:// with master branch",
			url:     "hf://org/repo/master/file.txt",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "master",
				FilePath:  "file.txt",
			},
		},
		{
			name:    "hf:// with nested path",
			url:     "hf://org/repo/models/v1/config.json",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "models/v1/config.json",
			},
		},
		{
			name:    "hf:// with main and nested path",
			url:     "hf://org/repo/main/models/v1/config.json",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "models/v1/config.json",
			},
		},
		// hf.co short form
		{
			name:    "hf.co short form",
			url:     "hf.co/org/repo/resolve/main/file.txt",
			wantErr: false,
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "file.txt",
			},
		},
		// Error cases
		{
			name:    "too few parts",
			url:     "huggingface.co/org/repo",
			wantErr: true,
		},
		{
			name:    "hf:// too few parts",
			url:     "hf://org",
			wantErr: true,
		},
		{
			name:    "empty string",
			url:     "",
			wantErr: true,
		},
		{
			name:    "just host",
			url:     "huggingface.co",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseHuggingFaceURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseHuggingFaceURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if result.Namespace != tt.expected.Namespace {
				t.Errorf("Namespace = %q, want %q", result.Namespace, tt.expected.Namespace)
			}
			if result.Repo != tt.expected.Repo {
				t.Errorf("Repo = %q, want %q", result.Repo, tt.expected.Repo)
			}
			if result.Branch != tt.expected.Branch {
				t.Errorf("Branch = %q, want %q", result.Branch, tt.expected.Branch)
			}
			if result.FilePath != tt.expected.FilePath {
				t.Errorf("FilePath = %q, want %q", result.FilePath, tt.expected.FilePath)
			}
		})
	}
}

func TestHuggingFaceURLString(t *testing.T) {
	url := &HuggingFaceURL{
		Namespace: "myorg",
		Repo:      "myrepo",
		Branch:    "main",
		FilePath:  "config.json",
	}

	expected := "https://huggingface.co/myorg/myrepo/resolve/main/config.json"
	if got := url.String(); got != expected {
		t.Errorf("String() = %q, want %q", got, expected)
	}
}

func TestHuggingFaceURLRepositoryID(t *testing.T) {
	url := &HuggingFaceURL{
		Namespace: "myorg",
		Repo:      "myrepo",
		Branch:    "main",
		FilePath:  "config.json",
	}

	expected := "myorg/myrepo"
	if got := url.RepositoryID(); got != expected {
		t.Errorf("RepositoryID() = %q, want %q", got, expected)
	}
}

func TestParseHuggingFaceURLRoundtrip(t *testing.T) {
	tests := []struct {
		name string
		url  *HuggingFaceURL
	}{
		{
			name: "simple",
			url: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "file.txt",
			},
		},
		{
			name: "nested path",
			url: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "develop",
				FilePath:  "models/v1/config.json",
			},
		},
		{
			name: "hyphenated names",
			url: &HuggingFaceURL{
				Namespace: "my-org",
				Repo:      "my-repo",
				Branch:    "feature-branch",
				FilePath:  "path/to/file.txt",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Convert to string and parse back
			urlStr := tt.url.String()
			parsed, err := ParseHuggingFaceURL(urlStr)
			if err != nil {
				t.Fatalf("Failed to parse URL %q: %v", urlStr, err)
			}

			if parsed.Namespace != tt.url.Namespace {
				t.Errorf("Namespace = %q, want %q", parsed.Namespace, tt.url.Namespace)
			}
			if parsed.Repo != tt.url.Repo {
				t.Errorf("Repo = %q, want %q", parsed.Repo, tt.url.Repo)
			}
			if parsed.Branch != tt.url.Branch {
				t.Errorf("Branch = %q, want %q", parsed.Branch, tt.url.Branch)
			}
			if parsed.FilePath != tt.url.FilePath {
				t.Errorf("FilePath = %q, want %q", parsed.FilePath, tt.url.FilePath)
			}
		})
	}
}

func TestParseHuggingFaceURLEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantErr  bool
		expected *HuggingFaceURL
	}{
		{
			name: "underscore in names",
			url:  "https://huggingface.co/my_org/my_repo/resolve/main/my_file.txt",
			expected: &HuggingFaceURL{
				Namespace: "my_org",
				Repo:      "my_repo",
				Branch:    "main",
				FilePath:  "my_file.txt",
			},
		},
		{
			name: "dots in filename",
			url:  "https://huggingface.co/org/repo/resolve/main/model.ckpt.safetensors",
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "model.ckpt.safetensors",
			},
		},
		{
			name: "special chars in path",
			url:  "https://huggingface.co/org/repo/resolve/main/path-with-dashes/file_with_underscores.txt",
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "path-with-dashes/file_with_underscores.txt",
			},
		},
		{
			name: "deeply nested path",
			url:  "https://huggingface.co/org/repo/resolve/main/a/b/c/d/e/f/g/file.txt",
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "repo",
				Branch:    "main",
				FilePath:  "a/b/c/d/e/f/g/file.txt",
			},
		},
		{
			name: "numeric repo name",
			url:  "https://huggingface.co/org/123/resolve/main/file.txt",
			expected: &HuggingFaceURL{
				Namespace: "org",
				Repo:      "123",
				Branch:    "main",
				FilePath:  "file.txt",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseHuggingFaceURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseHuggingFaceURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if result.Namespace != tt.expected.Namespace {
				t.Errorf("Namespace = %q, want %q", result.Namespace, tt.expected.Namespace)
			}
			if result.Repo != tt.expected.Repo {
				t.Errorf("Repo = %q, want %q", result.Repo, tt.expected.Repo)
			}
			if result.Branch != tt.expected.Branch {
				t.Errorf("Branch = %q, want %q", result.Branch, tt.expected.Branch)
			}
			if result.FilePath != tt.expected.FilePath {
				t.Errorf("FilePath = %q, want %q", result.FilePath, tt.expected.FilePath)
			}
		})
	}
}
