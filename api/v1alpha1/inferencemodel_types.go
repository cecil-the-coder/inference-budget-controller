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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InferenceModelSpec defines the desired state of InferenceModel
type InferenceModelSpec struct {
	// ModelName is the name of the model for API routing
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`

	// Memory is the memory requirement for this model (e.g., "80Gi")
	// +kubebuilder:validation:Pattern=^[0-9]+(Ki|Mi|Gi|Ti)$
	Memory string `json:"memory"`

	// BackendURL is the URL of the backend inference server
	// +kubebuilder:validation:Format=uri
	BackendURL string `json:"backendUrl"`

	// NodeSelector specifies which node pool this model should run on
	// +kubebuilder:default={"inference-pool": "default"}
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// CooldownPeriod is how long to wait before scaling down an idle model
	// +kubebuilder:default="10m"
	CooldownPeriod metav1.Duration `json:"cooldownPeriod,omitempty"`

	// MaxReplicas is the maximum number of replicas (default 1 for now)
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// Dependencies are other models that must be running before this one starts
	Dependencies []string `json:"dependencies,omitempty"`

	// ContainerImage is the Docker image for the model server
	ContainerImage string `json:"containerImage,omitempty"`

	// Resources defines the compute resources for the model container
	Resources ResourceRequirements `json:"resources,omitempty"`
}

// ResourceRequirements describes the compute resource requirements
type ResourceRequirements struct {
	// Requests are the minimum required resources
	Requests ResourceList `json:"requests,omitempty"`
	// Limits are the maximum allowed resources
	Limits ResourceList `json:"limits,omitempty"`
}

// ResourceList is a collection of resource requirements
type ResourceList struct {
	// CPU is the CPU requirement
	CPU string `json:"cpu,omitempty"`
	// Memory is the memory requirement
	Memory string `json:"memory,omitempty"`
	// GPU is the GPU requirement (e.g., "1" for one GPU)
	GPU string `json:"gpu,omitempty"`
}

// InferenceModelStatus defines the observed state of InferenceModel
type InferenceModelStatus struct {
	// Ready indicates if the model is ready to serve requests
	Ready bool `json:"ready"`

	// Replicas is the current number of replicas
	Replicas int32 `json:"replicas"`

	// DeclaredMemory is the memory declared in the spec (mirrored for status)
	DeclaredMemory string `json:"declaredMemory,omitempty"`

	// ObservedPeakMemory is the peak memory observed at runtime
	ObservedPeakMemory string `json:"observedPeakMemory,omitempty"`

	// UtilizationPercent is the ratio of observed to declared memory (0-100)
	UtilizationPercent int32 `json:"utilizationPercent,omitempty"`

	// Recommendation contains suggestions for right-sizing
	Recommendation string `json:"recommendation,omitempty"`

	// LastObservation is when the metrics were last updated
	LastObservation metav1.Time `json:"lastObservation,omitempty"`

	// Conditions represent the latest available observations
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=`.status.ready`
//+kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=`.status.replicas`
//+kubebuilder:printcolumn:name="Memory",type="string",JSONPath=`.spec.memory`
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
