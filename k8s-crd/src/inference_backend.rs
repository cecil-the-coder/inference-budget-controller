// k8s-crd/src/inference_backend.rs
//
// kube-rs CRD definition for InferenceBackend.
//
// ═══════════════════════════════════════════════════════════════════════════
// ARCHITECTURE CHANGE: InferenceBackend no longer describes a pod
// ═══════════════════════════════════════════════════════════════════════════
//
// In the original Go controller InferenceBackend was essentially a partial
// PodSpec: it held the container Image, Args, Env, Volumes, InitContainers,
// and SecurityContext that the controller would stamp into pods.
//
// In the new fox-merged architecture the fox engine is the process that runs
// on the node and is managed by the OS (systemd / container runtime), NOT by
// the Kubernetes pod scheduler.  Therefore InferenceBackend:
//
//   OLD role  → pod template fragment (container image + args)
//   NEW role  → serving configuration for fox:
//                 • which fox engine variant to use (llamacpp, vllm, …)
//                 • how fox exposes the model over HTTP
//                 • GPU assignment strategy
//                 • health probe paths
//                 • argument templates ($(HF_SOURCE), $(MODEL_DIR), etc.)
//
// The pod-level fields (InitContainers, Volumes, VolumeMounts,
// SecurityContext) are removed because fox owns those lifecycle concerns.
// A new `serving` section captures fox-specific parameters.
//
// InferenceModels still reference an InferenceBackend by name, which tells
// the controller which fox engine profile to activate for that model.
// ═══════════════════════════════════════════════════════════════════════════

use k8s_openapi::api::core::v1::EnvVar;
use k8s_openapi::apimachinery::pkg::apis::meta::v1::Condition;
use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::shared::{GPUConfig, ImageReference};

// ---------------------------------------------------------------------------
// InferenceBackend CRD root
// ---------------------------------------------------------------------------

/// InferenceBackend describes the serving configuration for a fox inference
/// engine instance.
///
/// One InferenceBackend resource represents one *class* of backend
/// (e.g. `llama-cpp-gpu`, `vllm-a100`).  Multiple InferenceModels can
/// reference the same backend; fox merges the model-specific parameters
/// (model path, context size) with the backend template at load time.
///
/// CRD group/version: `inference.eh-ops.io/v1alpha1`
/// kubectl short name: `ib`
/// Scope: Namespaced
#[derive(CustomResource, Clone, Debug, PartialEq, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "inference.eh-ops.io",
    version = "v1alpha1",
    kind = "InferenceBackend",
    plural = "inferencebackends",
    singular = "inferencebackend",
    shortname = "ib",
    namespaced,
    status = "InferenceBackendStatus",
    printcolumn = r#"{"name":"Engine","type":"string","jsonPath":".spec.engine"}"#,
    printcolumn = r#"{"name":"Port","type":"integer","jsonPath":".spec.port"}"#,
    printcolumn = r#"{"name":"Ready","type":"boolean","jsonPath":".status.ready"}"#,
    printcolumn = r#"{"name":"LoadedModels","type":"integer","jsonPath":".status.loadedModels"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct InferenceBackendSpec {
    // ---- Engine identity ----
    /// Which fox engine variant this backend uses.
    /// The controller maps this to a specific fox binary / plugin.
    pub engine: InferenceEngine,

    /// Container / binary image for the engine.
    /// For node-local fox deployments this is the OCI image that fox
    /// unpacks onto the node.  For sidecar-style deployments it is the
    /// container image used by the pod.
    pub image: ImageReference,

    /// TCP port the engine listens on for OpenAI-compatible requests.
    /// The proxy routes model traffic here.
    ///
    /// Default: `8080`
    #[serde(default = "default_port")]
    pub port: i32,

    // ---- Probe paths ----
    /// HTTP path for the engine's readiness probe.
    ///
    /// Default: `"/health"`
    #[serde(default = "default_readiness_path")]
    pub readiness_path: String,

    /// HTTP path for the engine's liveness probe.
    /// Defaults to `readiness_path`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub liveness_path: String,

    // ---- Argument templates ----
    /// CLI arguments passed to the engine binary.
    /// Supports variable substitution performed by fox at model-load time:
    ///   `$(HF_SOURCE)`   → absolute path to the downloaded model file/dir
    ///   `$(MODEL_DIR)`   → model cache directory
    ///   `$(MMPROJ_SOURCE)` → absolute path to the multimodal projector file
    ///   `$(PORT)`        → value of `spec.port`
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,

    /// Environment variables injected into the engine process.
    /// InferenceModel `spec.env` entries are merged on top of these
    /// (model-level wins on key collision).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub env: Vec<EnvVar>,

    // ---- GPU configuration ----
    /// Default GPU assignment strategy for models using this backend.
    /// Individual models can override this via `spec.backendOverrides.gpu`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gpu: Option<GPUConfig>,

    // ---- fox-specific serving parameters ----
    /// Extended serving configuration forwarded to fox.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub serving: Option<ServingConfig>,

    // ---- Node placement defaults ----
    /// Default node selector applied to models using this backend.
    /// Can be overridden at the InferenceModel level.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub node_selector: Option<std::collections::BTreeMap<String, String>>,
}

fn default_port() -> i32 {
    8080
}
fn default_readiness_path() -> String {
    "/health".to_owned()
}

// ---------------------------------------------------------------------------
// InferenceEngine
// ---------------------------------------------------------------------------

/// Identifies which fox engine plugin handles inference.
///
/// fox uses a plugin architecture; each variant maps to a shared library
/// loaded at runtime.  New variants can be added without changing the API.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
pub enum InferenceEngine {
    /// llama.cpp GGUF engine — the default for CPU and consumer GPU workloads.
    #[default]
    LlamaCpp,
    /// vLLM — PagedAttention-based engine for high-throughput GPU workloads.
    Vllm,
    /// Ollama-compatible engine for development / local testing.
    Ollama,
    /// Escape hatch: a custom engine identified by a string name.
    /// The controller passes this value to fox as `--engine <name>`.
    Custom(String),
}

// ---------------------------------------------------------------------------
// ServingConfig
// ---------------------------------------------------------------------------

/// Fox-specific serving parameters that are forwarded verbatim to the
/// fox daemon configuration.  All fields are optional; unset fields use
/// fox's compiled-in defaults.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ServingConfig {
    /// Maximum number of concurrent in-flight requests.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_concurrent_requests: Option<i32>,

    /// Default context window size for models loaded by this backend.
    /// Models can override this via `spec.source.hugging_face.context_size`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub default_context_size: Option<i32>,

    /// Enable continuous batching.  Default: `true` for vLLM, `false` for
    /// llama.cpp (where it depends on the build).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub continuous_batching: Option<bool>,

    /// Flash-attention mode: `"auto"`, `"on"`, `"off"`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub flash_attention: String,

    /// Maximum number of models held in GPU memory simultaneously before
    /// fox starts evicting idle models.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_loaded_models: Option<i32>,

    /// Model-level metadata visible to InferenceModel owners.
    /// Stored as arbitrary key-value pairs forwarded to fox.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub extra_params: Option<std::collections::BTreeMap<String, String>>,

    /// List of models that are always kept loaded ("pinned") on this backend,
    /// regardless of the global eviction budget.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub pinned_models: Vec<ServedModel>,
}

// ---------------------------------------------------------------------------
// ServedModel
// ---------------------------------------------------------------------------

/// A model identity within a backend's serving context.
/// Used to express "pin this model" or "pre-warm this model" within
/// `ServingConfig.pinned_models`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ServedModel {
    /// The `InferenceModel.spec.modelName` value.
    pub model_name: String,

    /// The `InferenceModel` resource name (optional — used for cross-referencing
    /// when model names are not globally unique within a namespace).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resource_name: String,
}

// ---------------------------------------------------------------------------
// InferenceBackendStatus
// ---------------------------------------------------------------------------

/// Observed state of an InferenceBackend.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct InferenceBackendStatus {
    /// `true` when the fox engine is running and the readiness probe passes.
    pub ready: bool,

    /// Number of InferenceModel resources currently loaded by this backend.
    #[serde(default)]
    pub loaded_models: i32,

    /// Total memory currently consumed across all loaded models (in bytes).
    /// Updated by the controller from the metrics API.
    #[serde(default)]
    pub used_memory_bytes: i64,

    /// Human-readable engine version reported by fox (e.g. `"llama.cpp b3800"`).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub engine_version: String,

    /// Names of InferenceModel resources currently loaded on this backend.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub loaded_model_names: Vec<String>,

    /// Standard Kubernetes condition list.
    /// Known types: `Ready`, `EngineHealthy`, `GpuAvailable`.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub conditions: Vec<Condition>,
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;
    use crate::shared::ImageReference;

    fn minimal_spec() -> InferenceBackendSpec {
        InferenceBackendSpec {
            engine: InferenceEngine::LlamaCpp,
            image: ImageReference {
                repository: "ghcr.io/ggml-org/llama.cpp".into(),
                tag: "server".into(),
                digest: String::new(),
            },
            port: 8080,
            readiness_path: "/health".into(),
            liveness_path: String::new(),
            args: vec!["--host".into(), "0.0.0.0".into(), "--port".into(), "$(PORT)".into()],
            env: vec![],
            gpu: None,
            serving: None,
            node_selector: None,
        }
    }

    #[test]
    fn spec_roundtrip() {
        let spec = minimal_spec();
        let json = serde_json::to_string_pretty(&spec).unwrap();
        let spec2: InferenceBackendSpec = serde_json::from_str(&json).unwrap();
        assert_eq!(spec, spec2);
    }

    #[test]
    fn status_default_not_ready() {
        let s = InferenceBackendStatus::default();
        assert!(!s.ready);
        assert_eq!(s.loaded_models, 0);
    }

    #[test]
    fn engine_custom_roundtrip() {
        let e = InferenceEngine::Custom("my-engine-v2".into());
        let s = serde_json::to_string(&e).unwrap();
        let e2: InferenceEngine = serde_json::from_str(&s).unwrap();
        assert_eq!(e, e2);
    }

    #[test]
    fn engine_llamacpp_serialises_as_string_variant() {
        let e = InferenceEngine::LlamaCpp;
        let s = serde_json::to_string(&e).unwrap();
        // serde's default for unit variants is the variant name as a string
        assert_eq!(s, r#""LlamaCpp""#);
    }
}
