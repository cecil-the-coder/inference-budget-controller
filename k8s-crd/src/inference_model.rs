// k8s-crd/src/inference_model.rs
//
// kube-rs CRD definition for InferenceModel.
//
// Maps from api/v1alpha1/inferencemodel_types.go.
//
// Design decisions
// ─────────────────
// 1. `corev1::*` types (EnvVar, Toleration, Affinity, etc.) are used verbatim
//    from k8s-openapi — they already implement Serialize + Deserialize +
//    JsonSchema.
// 2. All quantity-style fields (memory, cpu) use `crate::quantity::Quantity`
//    so that budget arithmetic can be done without string manipulation.
// 3. `ServiceType` from k8s-openapi is a `String` newtype with well-known
//    constants (ClusterIP, NodePort, LoadBalancer).  We use it directly.
// 4. Time fields use `k8s_openapi::apimachinery::pkg::apis::meta::v1::Time`
//    which is a newtype over chrono::DateTime<Utc>.
// 5. `metav1::Condition` is used for the standard conditions list.

use k8s_openapi::api::core::v1::{
    Affinity, Container, ContainerPort, EnvVar, ResourceRequirements, SecurityContext,
    ServiceType, Toleration, Volume, VolumeMount,
};
use k8s_openapi::apimachinery::pkg::apis::meta::v1::{Condition, Time};
use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::quantity::Quantity;
use crate::shared::{GPUConfig, ImageReference};

// ---------------------------------------------------------------------------
// InferenceModel CRD root
// ---------------------------------------------------------------------------

/// InferenceModel describes a single LLM or multimodal model to be served.
///
/// The controller:
///   1. Downloads the model weights (HuggingFace, PVC, or URL source).
///   2. Schedules a pod on a node that has an InferenceBackend matching
///      `spec.backend`.
///   3. Tracks memory budget across all loaded models on each node.
///   4. Scales to zero after `spec.scaling.scale_to_zero_delay` of inactivity.
///
/// CRD group/version: `inference.eh-ops.io/v1alpha1`
/// kubectl short name: `im`
/// Scope: Namespaced
#[derive(CustomResource, Clone, Debug, PartialEq, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "inference.eh-ops.io",
    version = "v1alpha1",
    kind = "InferenceModel",
    plural = "inferencemodels",
    singular = "inferencemodel",
    shortname = "im",
    namespaced,
    status = "InferenceModelStatus",
    printcolumn = r#"{"name":"Phase","type":"string","jsonPath":".status.phase"}"#,
    printcolumn = r#"{"name":"Ready","type":"boolean","jsonPath":".status.ready"}"#,
    printcolumn = r#"{"name":"Replicas","type":"integer","jsonPath":".status.replicas"}"#,
    printcolumn = r#"{"name":"Memory","type":"string","jsonPath":".spec.resources.memory"}"#,
    printcolumn = r#"{"name":"Backend","type":"string","jsonPath":".spec.backend"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct InferenceModelSpec {
    /// API-level model name used for routing (e.g. `"llama3-8b"`).
    /// This becomes the `model` field in OpenAI-compatible requests.
    pub model_name: String,

    /// Name of the `InferenceBackend` resource (in the same namespace) that
    /// defines which inference engine to use.
    pub backend: String,

    /// Where the model weights come from.
    pub source: ModelSource,

    /// Storage / caching configuration for the model weights.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub storage: Option<StorageConfig>,

    /// Compute resource requirements and memory budget declaration.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub resources: Option<ModelResources>,

    /// Auto-scaling / scale-to-zero configuration.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub scaling: Option<ScalingConfig>,

    // ---- Pod placement ----
    /// Node selector labels.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub node_selector: Option<std::collections::BTreeMap<String, String>>,

    /// Pod tolerations.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tolerations: Vec<Toleration>,

    /// Pod affinity / anti-affinity rules.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub affinity: Option<Affinity>,

    // ---- Networking ----
    /// Kubernetes Service configuration for this model.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub service: Option<ServiceConfig>,

    /// Gateway API `HTTPRoute` configuration.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub http_route: Option<HTTPRouteConfig>,

    // ---- Container overrides ----
    /// Sidecar container to inject alongside the inference engine.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sidecar: Option<SidecarConfig>,

    /// Extra environment variables merged on top of the backend's env list.
    /// Later entries win on key collision.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub env: Vec<EnvVar>,

    /// Additional CLI arguments appended to the backend's arg list.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,

    /// Per-model overrides for backend settings (image, port, GPU, etc.).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub backend_overrides: Option<BackendOverrides>,
}

// ---------------------------------------------------------------------------
// InferenceModelStatus
// ---------------------------------------------------------------------------

/// Observed state of an InferenceModel.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct InferenceModelStatus {
    // ---- Lifecycle ----
    /// High-level lifecycle phase: `Pending | Downloading | Ready |
    /// ScaledToZero | Failed`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub phase: String,

    // ---- Download progress ----
    /// Download phase for the model weights.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub download_phase: Option<DownloadPhase>,

    /// Aggregate download progress 0–100.
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub download_progress: i32,

    /// Human-readable download message.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub download_message: String,

    /// Total bytes to download.
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub download_bytes_total: i64,

    /// Bytes downloaded so far.
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub download_bytes_done: i64,

    /// Current download speed in bytes/second.
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub download_speed: i64,

    /// Estimated time remaining (human-readable, e.g. `"3m42s"`).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub download_eta: String,

    /// Per-file download status.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub download_files: Vec<FileDownloadStatus>,

    /// Error message if the download failed.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub download_error: String,

    /// When the download started.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub download_started_at: Option<Time>,

    /// When the download completed.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub download_completed_at: Option<Time>,

    // ---- Serving ----
    /// `true` when the model is fully loaded and ready to handle requests.
    pub ready: bool,

    /// Current number of pod replicas (0 when scaled to zero).
    pub replicas: i32,

    /// Number of replicas that are `Ready`.
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub available_replicas: i32,

    // ---- Budget tracking ----
    /// Memory as declared in `spec.resources.memory` (copied here for
    /// display and budget validation without reading the spec).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub declared_memory: Option<Quantity>,

    /// Peak memory observed via the metrics API at runtime.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub observed_peak_memory: Option<Quantity>,

    /// `observed_peak_memory / declared_memory * 100` (0–100+).
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub utilization_percent: i32,

    /// Human-readable right-sizing recommendation.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recommendation: String,

    // ---- Activity ----
    /// Timestamp of the most recent request handled by this model.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_request: Option<Time>,

    // ---- Standard conditions ----
    /// Standard Kubernetes condition list.
    /// Known types: `Ready`, `Downloading`, `MemoryBudgetExceeded`.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub conditions: Vec<Condition>,
}

fn is_zero_i32(v: &i32) -> bool {
    *v == 0
}
fn is_zero_i64(v: &i64) -> bool {
    *v == 0
}

// ---------------------------------------------------------------------------
// DownloadPhase
// ---------------------------------------------------------------------------

/// Lifecycle phase of the model weight download.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
pub enum DownloadPhase {
    Pending,
    Downloading,
    Complete,
    Failed,
}

// ---------------------------------------------------------------------------
// FileDownloadStatus
// ---------------------------------------------------------------------------

/// Status of a single file being downloaded.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct FileDownloadStatus {
    /// Relative path within the repository / PVC.
    pub path: String,
    /// One of `Pending | Downloading | Complete | Failed`.
    pub phase: String,
    /// Bytes downloaded for this file.
    pub bytes_done: i64,
    /// Total size of this file in bytes.
    pub bytes_total: i64,
}

// ---------------------------------------------------------------------------
// ModelSource
// ---------------------------------------------------------------------------

/// Where the model weights come from.  Exactly one of the optional fields
/// should be set; the controller treats any other combination as invalid.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ModelSource {
    /// Download from HuggingFace Hub.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hugging_face: Option<HuggingFaceSource>,

    /// Use model weights already present on a PVC.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pvc: Option<PVCSource>,

    /// Download from an arbitrary URL (future use).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub url: Option<URLSource>,
}

// ---------------------------------------------------------------------------
// HuggingFaceSource
// ---------------------------------------------------------------------------

/// Download model weights from HuggingFace Hub.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct HuggingFaceSource {
    /// HuggingFace repository, e.g. `"meta-llama/Llama-3.1-8B-Instruct"`.
    pub repo: String,

    /// Glob patterns of specific files to download.
    /// Empty means download everything.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub files: Vec<String>,

    /// Specific model file within the repo (e.g. `"model.gguf"`).
    /// When set, `$(HF_SOURCE)` in backend args resolves to this file
    /// instead of the directory root.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model_file: String,

    /// Multimodal projector file (e.g. `"mmproj-model-f16.gguf"`).
    /// Exposed as `$(MMPROJ_SOURCE)` in backend args.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mmproj_file: String,

    /// Context window size hint (e.g. `"131072"`).
    /// Informational — some backends read this from the model; others need
    /// it passed explicitly via args.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub context_size: String,

    /// Name of the `Secret` containing the HuggingFace API token.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub token_secret: String,

    /// Key within `token_secret` that holds the token value.
    /// Defaults to `"token"`.
    #[serde(default = "default_token_secret_key", skip_serializing_if = "String::is_empty")]
    pub token_secret_key: String,

    /// Specific git revision (branch, tag, or commit SHA) to pin downloads to.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub revision: String,
}

fn default_token_secret_key() -> String {
    "token".to_owned()
}

// ---------------------------------------------------------------------------
// PVCSource
// ---------------------------------------------------------------------------

/// Use model weights already stored on a PersistentVolumeClaim.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct PVCSource {
    /// PVC name (must be in the same namespace).
    pub name: String,

    /// Path within the PVC to the model directory or file.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub path: String,

    /// Specific model file within `path` (for sharded GGUF models).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model_file: String,

    /// Multimodal projector file within `path`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mmproj_file: String,
}

// ---------------------------------------------------------------------------
// URLSource
// ---------------------------------------------------------------------------

/// Download model weights from an arbitrary URL.
/// Reserved for future use; not fully implemented.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct URLSource {
    pub url: String,
}

// ---------------------------------------------------------------------------
// StorageConfig
// ---------------------------------------------------------------------------

/// How model weights are cached between restarts.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct StorageConfig {
    /// Name of an existing PVC to use as the weight cache.
    /// If `create` is true and this is empty a new PVC is created by the
    /// controller.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub pvc: String,

    /// When `true` the controller creates a PVC on first use.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub create: bool,

    /// Size of the PVC to create, e.g. `"100Gi"`.
    /// Only meaningful when `create` is `true`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<Quantity>,

    /// StorageClass for the new PVC.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub storage_class: String,

    /// Sub-directory within the PVC for this model's weights.
    /// Defaults to the InferenceModel name when empty.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model_dir: String,

    /// When `true`, the PVC is shared across multiple models using a
    /// common cache directory layout (`<model_dir>/<model-name>/`).
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub shared: bool,
}

// ---------------------------------------------------------------------------
// ModelResources
// ---------------------------------------------------------------------------

/// Compute resource requirements and memory budget declaration.
///
/// `memory` is the single authoritative field for admission control: the
/// controller sums `memory` across all `Ready` InferenceModels on a node and
/// rejects new models when the sum would exceed the node's allocatable memory.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ModelResources {
    /// Declared memory footprint, e.g. `"80Gi"`.
    /// Used for budget tracking and admission control.
    pub memory: Quantity,

    /// CPU request, e.g. `"2"` or `"500m"`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cpu: Option<Quantity>,

    /// Actual container memory limit.
    /// Defaults to `memory` when not set.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub memory_limit: Option<Quantity>,
}

// ---------------------------------------------------------------------------
// EvictionPolicy
// ---------------------------------------------------------------------------

/// Controls when an idle model may be evicted from a node.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
pub enum EvictionPolicy {
    /// Allow both idle-timeout eviction and memory-pressure eviction.
    #[default]
    Default,
    /// Only evict when another model needs the memory.
    /// Idle models remain loaded indefinitely.
    MemoryPressureOnly,
}

// ---------------------------------------------------------------------------
// ScalingConfig
// ---------------------------------------------------------------------------

/// Auto-scaling and scale-to-zero configuration.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ScalingConfig {
    /// Enable or disable auto-scaling.  Defaults to `true`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub enabled: Option<bool>,

    /// Maximum number of replicas (0–10).  Defaults to `1`.
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub max_replicas: i32,

    /// How long to wait after the last request before starting scale-to-zero.
    /// Parsed as a Go duration string, e.g. `"10m"` or `"1h30m"`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub cooldown_period: String,

    /// Alias for `cooldown_period`; takes precedence if both are set.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub scale_to_zero_delay: String,

    /// Eviction policy for running replicas.
    #[serde(default)]
    pub eviction_policy: EvictionPolicy,
}

// ---------------------------------------------------------------------------
// ServiceConfig
// ---------------------------------------------------------------------------

/// Kubernetes Service settings for this model.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ServiceConfig {
    /// Service type.  One of `ClusterIP`, `NodePort`, `LoadBalancer`.
    /// Defaults to `ClusterIP`.
    #[serde(default = "default_service_type")]
    pub r#type: ServiceType,

    /// Service port.  Defaults to the backend's `port`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub port: Option<i32>,

    /// Annotations to add to the generated Service.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub annotations: Option<std::collections::BTreeMap<String, String>>,
}

fn default_service_type() -> ServiceType {
    ServiceType("ClusterIP".to_owned())
}

// ---------------------------------------------------------------------------
// HTTPRouteConfig
// ---------------------------------------------------------------------------

/// Gateway API `HTTPRoute` configuration.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct HTTPRouteConfig {
    /// Create an `HTTPRoute` resource for this model.
    pub enabled: bool,

    /// Hostnames the route matches.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub hostnames: Vec<String>,

    /// Parent Gateway references in `"[namespace/]name"` format.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub parent_refs: Vec<String>,
}

// ---------------------------------------------------------------------------
// SidecarConfig
// ---------------------------------------------------------------------------

/// An extra container injected into the model pod alongside the inference
/// engine.  Useful for token-counting proxies, telemetry agents, etc.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct SidecarConfig {
    /// Container name.  Defaults to the InferenceModel name + `"-sidecar"`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,

    /// Container image, e.g. `"ghcr.io/myorg/proxy:v1"`.
    pub image: String,

    /// Ports the sidecar exposes.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub ports: Vec<ContainerPort>,

    /// Sidecar CLI arguments.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,

    /// Sidecar environment variables.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub env: Vec<EnvVar>,

    /// Resource requests and limits for the sidecar.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub resources: Option<ResourceRequirements>,
}

// ---------------------------------------------------------------------------
// BackendOverrides
// ---------------------------------------------------------------------------

/// Per-model overrides for settings inherited from the parent InferenceBackend.
/// Only the fields present here are overridden; all others use the backend's
/// value.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct BackendOverrides {
    /// Override the backend image.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image: Option<ImageReference>,

    /// Override the inference engine port.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub port: Option<i32>,

    /// Override the readiness probe HTTP path.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub readiness_path: Option<String>,

    /// Override the GPU configuration.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gpu: Option<GPUConfig>,
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    fn minimal_spec() -> InferenceModelSpec {
        InferenceModelSpec {
            model_name: "llama3".into(),
            backend: "llama-cpp".into(),
            source: ModelSource {
                hugging_face: Some(HuggingFaceSource {
                    repo: "meta-llama/Meta-Llama-3-8B-Instruct".into(),
                    ..Default::default()
                }),
                ..Default::default()
            },
            storage: None,
            resources: Some(ModelResources {
                memory: Quantity::new("80Gi"),
                cpu: None,
                memory_limit: None,
            }),
            scaling: None,
            node_selector: None,
            tolerations: vec![],
            affinity: None,
            service: None,
            http_route: None,
            sidecar: None,
            env: vec![],
            args: vec![],
            backend_overrides: None,
        }
    }

    #[test]
    fn spec_roundtrip() {
        let spec = minimal_spec();
        let json = serde_json::to_string_pretty(&spec).unwrap();
        let spec2: InferenceModelSpec = serde_json::from_str(&json).unwrap();
        assert_eq!(spec, spec2);
    }

    #[test]
    fn status_default_is_not_ready() {
        let status = InferenceModelStatus::default();
        assert!(!status.ready);
        assert_eq!(status.replicas, 0);
    }

    #[test]
    fn eviction_policy_serialises_as_string() {
        let p = EvictionPolicy::MemoryPressureOnly;
        let s = serde_json::to_string(&p).unwrap();
        assert_eq!(s, r#""MemoryPressureOnly""#);
    }
}
