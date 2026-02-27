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

package v1alpha1

// ImageReference defines a container image reference
type ImageReference struct {
	// Repository is the image repository
	Repository string `json:"repository,omitempty"`
	// Tag is the image tag
	Tag string `json:"tag,omitempty"`
	// Digest is the image digest (overrides tag if set)
	Digest string `json:"digest,omitempty"`
}

// GPUConfig defines GPU scheduling configuration
type GPUConfig struct {
	// Exclusive claims an exclusive GPU (amd.com/gpu: 1)
	Exclusive bool `json:"exclusive,omitempty"`
	// Shared claims a shared GPU (amd.com/gpu-shared: 1)
	Shared bool `json:"shared,omitempty"`
	// ResourceName allows custom GPU resource names
	ResourceName string `json:"resourceName,omitempty"`
}

// GetImage returns the full image reference
func (i *ImageReference) GetImage() string {
	if i == nil {
		return ""
	}
	if i.Digest != "" {
		return i.Repository + "@" + i.Digest
	}
	if i.Tag != "" {
		return i.Repository + ":" + i.Tag
	}
	return i.Repository
}

// HasImage returns true if an image reference is set
func (i *ImageReference) HasImage() bool {
	return i != nil && i.Repository != ""
}
