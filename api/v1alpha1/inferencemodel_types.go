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

// InferenceModelSpec defines the desired state of InferenceModel
type InferenceModelSpec struct {
	// ModelName is the name of the model for API routing
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`

	// Backend is the name of the InferenceBackend to use
	// +kubebuilder:validation:MinLength=1
	Backend string `json:"backend"`

	// Source defines where the model comes from
	Source ModelSource `json:"source"`

	// Storage configuration for model caching
	Storage StorageConfig `json:"storage,omitempty"`

	// Resources defines the compute resource requirements
	Resources ModelResources `json:"resources,omitempty"`

	// Scaling configuration
	Scaling ScalingConfig `json:"scaling,omitempty"`

	// Pod placement configuration
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`

	// Service configuration
	Service ServiceConfig `json:"service,omitempty"`

	// HTTPRoute configuration for Gateway API
	HTTPRoute *HTTPRouteConfig `json:"httpRoute,omitempty"`

	// Sidecar container configuration
	Sidecar *SidecarConfig `json:"sidecar,omitempty"`

	// Environment variables to add/override from backend
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Additional args to append to backend args
	Args []string `json:"args,omitempty"`

	// BackendOverrides allows overriding specific backend settings
	BackendOverrides *BackendOverrides `json:"backendOverrides,omitempty"`
}

// BackendOverrides allows overriding backend settings per-model
type BackendOverrides struct {
	// Image overrides the backend image
	Image *ImageReference `json:"image,omitempty"`

	// Port overrides the backend port
	Port *int32 `json:"port,omitempty"`

	// ReadinessPath overrides the readiness probe path
	ReadinessPath *string `json:"readinessPath,omitempty"`

	// GPU overrides the GPU configuration
	GPU *GPUConfig `json:"gpu,omitempty"`
}

// ModelSource defines where the model comes from
type ModelSource struct {
	// HuggingFace model source
	HuggingFace *HuggingFaceSource `json:"huggingFace,omitempty"`

	// PVC source (model already on a PVC)
	PVC *PVCSource `json:"pvc,omitempty"`

	// URL source (direct download, for future use)
	URL *URLSource `json:"url,omitempty"`
}

// HuggingFaceSource defines a HuggingFace model source
type HuggingFaceSource struct {
	// Repo is the HuggingFace repository (e.g., "meta-llama/Llama-3.1-8B-Instruct")
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// Files are specific files to download (glob patterns)
	// If empty, downloads all files
	Files []string `json:"files,omitempty"`

	// ModelFile is the specific model file to use (for sharded GGUF models)
	// If set, $(HF_SOURCE) in backend args will point to this file instead of the directory
	ModelFile string `json:"modelFile,omitempty"`

	// MmprojFile is the multimodal projector file (for vision models)
	// If set, $(MMPROJ_SOURCE) in backend args will point to this file
	MmprojFile string `json:"mmprojFile,omitempty"`

	// ContextSize is the context window size (e.g., "131072" for 128k)
	ContextSize string `json:"contextSize,omitempty"`

	// TokenSecret is the name of the secret containing the HuggingFace token
	TokenSecret string `json:"tokenSecret,omitempty"`

	// TokenSecretKey is the key in the secret containing the token
	// +kubebuilder:default=token
	TokenSecretKey string `json:"tokenSecretKey,omitempty"`

	// Revision is the specific revision to download (branch, tag, commit)
	Revision string `json:"revision,omitempty"`
}

// PVCSource defines a PVC-based model source
type PVCSource struct {
	// Name is the PVC name
	Name string `json:"name"`

	// Path is the path within the PVC to the model
	Path string `json:"path,omitempty"`

	// ModelFile is the specific model file (for sharded models)
	ModelFile string `json:"modelFile,omitempty"`

	// MmprojFile is the multimodal projector file (for vision models)
	MmprojFile string `json:"mmprojFile,omitempty"`
}

// URLSource defines a URL-based model source (for future use)
type URLSource struct {
	URL string `json:"url"`
}

// StorageConfig defines model storage configuration
type StorageConfig struct {
	// PVC references an existing PVC for model caching
	// If Create is true, a new PVC will be created
	PVC string `json:"pvc,omitempty"`

	// Create controls whether to create a new PVC
	Create bool `json:"create,omitempty"`

	// Size is the size of the PVC to create (only used if Create is true)
	Size string `json:"size,omitempty"`

	// StorageClass is the storage class to use (only used if Create is true)
	StorageClass string `json:"storageClass,omitempty"`

	// ModelDir is the subdirectory within the PVC for this model
	// Defaults to the model name if not specified
	ModelDir string `json:"modelDir,omitempty"`

	// Shared controls whether the PVC is shared across models
	// If true, the controller will use a common cache directory structure
	Shared bool `json:"shared,omitempty"`
}

// ModelResources defines the resource requirements and budget
type ModelResources struct {
	// Memory is the memory requirement for budget tracking (e.g., "80Gi")
	// This is used for admission control - model won't start if budget exceeded
	Memory string `json:"memory"`

	// CPU is the CPU request (optional)
	CPU string `json:"cpu,omitempty"`

	// MemoryLimit is the actual memory limit (if different from request)
	// If not set, defaults to Memory
	MemoryLimit string `json:"memoryLimit,omitempty"`
}

// ScalingConfig defines scaling behavior
type ScalingConfig struct {
	// Enabled controls whether auto-scaling is enabled
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// MaxReplicas is the maximum number of replicas
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// CooldownPeriod is how long to wait before scaling to zero
	CooldownPeriod string `json:"cooldownPeriod,omitempty"`

	// ScaleToZeroDelay is how long after last request before scaling to zero
	// Defaults to CooldownPeriod if not set
	ScaleToZeroDelay string `json:"scaleToZeroDelay,omitempty"`
}

// ServiceConfig defines the service configuration
type ServiceConfig struct {
	// Type is the service type
	// +kubebuilder:default=ClusterIP
	Type corev1.ServiceType `json:"type,omitempty"`

	// Port is the service port (defaults to backend port)
	Port *int32 `json:"port,omitempty"`

	// Annotations to add to the service
	Annotations map[string]string `json:"annotations,omitempty"`
}

// HTTPRouteConfig defines Gateway API HTTPRoute configuration
type HTTPRouteConfig struct {
	// Enabled controls whether to create an HTTPRoute
	Enabled bool `json:"enabled"`

	// Hostnames are the hostnames to match
	Hostnames []string `json:"hostnames,omitempty"`

	// ParentRefs are the Gateway references
	// Format: "namespace/name" or just "name" (uses same namespace)
	ParentRefs []string `json:"parentRefs,omitempty"`
}

// SidecarConfig defines sidecar container configuration
type SidecarConfig struct {
	// Name is the sidecar container name
	Name string `json:"name,omitempty"`

	// Image is the sidecar container image
	Image string `json:"image"`

	// Ports are the ports the sidecar exposes
	Ports []corev1.ContainerPort `json:"ports,omitempty"`

	// Args are the sidecar container arguments
	Args []string `json:"args,omitempty"`

	// Env are the sidecar container environment variables
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources are the sidecar container resource requirements
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// DownloadPhase describes the phase of model download
type DownloadPhase string

const (
	DownloadPhasePending     DownloadPhase = "Pending"
	DownloadPhaseDownloading DownloadPhase = "Downloading"
	DownloadPhaseComplete    DownloadPhase = "Complete"
	DownloadPhaseFailed      DownloadPhase = "Failed"
)

// InferenceModelStatus defines the observed state of InferenceModel
type InferenceModelStatus struct {
	// Phase is the current lifecycle phase
	Phase string `json:"phase,omitempty"`

	// DownloadPhase indicates the state of model download
	DownloadPhase DownloadPhase `json:"downloadPhase,omitempty"`

	// DownloadProgress is the download progress (0-100)
	DownloadProgress int32 `json:"downloadProgress,omitempty"`

	// DownloadMessage contains details about the download
	DownloadMessage string `json:"downloadMessage,omitempty"`

	// Ready indicates if the model is ready to serve requests
	Ready bool `json:"ready"`

	// Replicas is the current number of replicas
	Replicas int32 `json:"replicas"`

	// AvailableReplicas is the number of available replicas
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// DeclaredMemory is the memory declared in the spec (for budget tracking)
	DeclaredMemory string `json:"declaredMemory,omitempty"`

	// ObservedPeakMemory is the peak memory observed at runtime
	ObservedPeakMemory string `json:"observedPeakMemory,omitempty"`

	// UtilizationPercent is the ratio of observed to declared memory (0-100)
	UtilizationPercent int32 `json:"utilizationPercent,omitempty"`

	// Recommendation contains suggestions for right-sizing
	Recommendation string `json:"recommendation,omitempty"`

	// LastRequest is when the last request was received
	LastRequest *metav1.Time `json:"lastRequest,omitempty"`

	// Conditions represent the latest available observations
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=`.status.ready`
//+kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=`.status.replicas`
//+kubebuilder:printcolumn:name="Memory",type="string",JSONPath=`.spec.resources.memory`
//+kubebuilder:printcolumn:name="Backend",type="string",JSONPath=`.spec.backend`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`

// InferenceModel is the Schema for the inferencemodels API
type InferenceModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferenceModelSpec   `json:"spec,omitempty"`
	Status InferenceModelStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// InferenceModelList contains a list of InferenceModel
type InferenceModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceModel{}, &InferenceModelList{})
}
