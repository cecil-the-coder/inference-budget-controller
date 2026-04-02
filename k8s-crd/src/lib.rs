// k8s-crd/src/lib.rs
//
// Public re-exports for the k8s-crd crate.  Every module is `pub` so that
// downstream crates (the controller, the proxy sidecar) can do:
//
//   use k8s_crd::inference_model::{InferenceModel, InferenceModelSpec, …};
//   use k8s_crd::inference_backend::{InferenceBackend, …};
//   use k8s_crd::shared::{ImageReference, GPUConfig, Quantity};

pub mod inference_model;
pub mod inference_backend;
pub mod shared;
pub mod quantity;
pub mod scheme;

// Convenience flat re-exports so callers can write `k8s_crd::InferenceModel`
// without knowing which sub-module owns the type.
pub use inference_model::{
    InferenceModel, InferenceModelList, InferenceModelSpec, InferenceModelStatus,
    ModelSource, HuggingFaceSource, PVCSource, URLSource,
    StorageConfig, ModelResources, ScalingConfig, EvictionPolicy,
    ServiceConfig, HTTPRouteConfig, SidecarConfig, BackendOverrides,
    FileDownloadStatus, DownloadPhase,
};

pub use inference_backend::{
    InferenceBackend, InferenceBackendList, InferenceBackendSpec, InferenceBackendStatus,
    InferenceEngine, ServingConfig, ServedModel,
};

pub use shared::{ImageReference, GPUConfig};
pub use quantity::Quantity;
