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

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InferenceBackendSpec defines the desired state of InferenceBackend
type InferenceBackendSpec struct {
	// Image is the container image for this backend
	Image ImageReference `json:"image"`

	// Port is the port the backend listens on
	// +kubebuilder:default=8080
	Port int32 `json:"port,omitempty"`

	// Command is the container command (entrypoint)
	Command []string `json:"command,omitempty"`

	// Args are the container arguments
	// Supports variable substitution: $(HF_SOURCE), $(MODEL_DIR), $(PORT)
	Args []string `json:"args,omitempty"`

	// ReadinessPath is the HTTP path for readiness probes
	// +kubebuilder:default=/health
	ReadinessPath string `json:"readinessPath,omitempty"`

	// LivenessPath is the HTTP path for liveness probes (defaults to ReadinessPath)
	LivenessPath string `json:"livenessPath,omitempty"`

	// Env are environment variables to set
	Env []corev1.EnvVar `json:"env,omitempty"`

	// SecurityContext for the container
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// Volumes to add to the pod
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// VolumeMounts for the container
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// InitContainers to add before the main container
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// GPU configures GPU resource claims
	GPU *GPUConfig `json:"gpu,omitempty"`
}

// InferenceBackendStatus defines the observed state of InferenceBackend
type InferenceBackendStatus struct {
	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Image",type="string",JSONPath=`.spec.image.repository`
//+kubebuilder:printcolumn:name="Port",type="integer",JSONPath=`.spec.port`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`

// InferenceBackend is the Schema for the inferencebackends API
type InferenceBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferenceBackendSpec   `json:"spec,omitempty"`
	Status InferenceBackendStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// InferenceBackendList contains a list of InferenceBackend
type InferenceBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceBackend `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceBackend{}, &InferenceBackendList{})
}
