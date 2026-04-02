// k8s-crd/src/shared.rs
//
// Types that are shared between InferenceModel and InferenceBackend, mapped
// from api/v1alpha1/shared_types.go.
//
// Note on k8s-openapi types:
//   We re-use k8s_openapi::api::core::v1::{EnvVar, Toleration, Affinity,
//   Volume, VolumeMount, Container, ContainerPort, SecurityContext,
//   ResourceRequirements, ServiceType} verbatim — they already derive
//   Serialize, Deserialize and JsonSchema.

use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

// ---------------------------------------------------------------------------
// ImageReference
// ---------------------------------------------------------------------------

/// A container image reference broken into repository, tag and digest parts.
///
/// Maps from shared_types.go `ImageReference`.
///
/// The full image string is produced by `ImageReference::to_image_string()`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ImageReference {
    /// OCI image repository, e.g. `"ghcr.io/ggml-org/llama.cpp"`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub repository: String,

    /// Image tag, e.g. `"server-b3800"`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub tag: String,

    /// OCI digest (`sha256:…`). When set this overrides `tag`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub digest: String,
}

impl ImageReference {
    /// Returns `true` if `repository` is non-empty.
    pub fn has_image(&self) -> bool {
        !self.repository.is_empty()
    }

    /// Produces the full image reference string.
    ///
    /// ```
    /// use k8s_crd::shared::ImageReference;
    ///
    /// let r = ImageReference {
    ///     repository: "ghcr.io/ggml-org/llama.cpp".into(),
    ///     tag: "server".into(),
    ///     digest: String::new(),
    /// };
    /// assert_eq!(r.to_image_string(), "ghcr.io/ggml-org/llama.cpp:server");
    /// ```
    pub fn to_image_string(&self) -> String {
        if !self.digest.is_empty() {
            return format!("{}@{}", self.repository, self.digest);
        }
        if !self.tag.is_empty() {
            return format!("{}:{}", self.repository, self.tag);
        }
        self.repository.clone()
    }
}

// ---------------------------------------------------------------------------
// GPUConfig
// ---------------------------------------------------------------------------

/// GPU scheduling configuration.
///
/// Maps from shared_types.go `GPUConfig`.
///
/// Exactly one of `exclusive`, `shared`, or `resource_name` should be set.
/// When `exclusive` is true, the controller injects `amd.com/gpu: "1"` into
/// the pod's resource requests (or `nvidia.com/gpu: "1"` — configurable via
/// `resource_name`).
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct GPUConfig {
    /// Claim an exclusive GPU (`amd.com/gpu: "1"` or the value of
    /// `resource_name`).
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub exclusive: bool,

    /// Claim a shared GPU slice (`amd.com/gpu-shared: "1"`).
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub shared: bool,

    /// Custom GPU resource name, e.g. `"nvidia.com/gpu"`.
    /// Overrides the default `amd.com/gpu` when `exclusive` is true.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resource_name: String,
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn image_digest_wins_over_tag() {
        let r = ImageReference {
            repository: "reg.io/img".into(),
            tag: "v1".into(),
            digest: "sha256:abc".into(),
        };
        assert_eq!(r.to_image_string(), "reg.io/img@sha256:abc");
    }

    #[test]
    fn image_tag_fallback() {
        let r = ImageReference {
            repository: "reg.io/img".into(),
            tag: "v2".into(),
            digest: String::new(),
        };
        assert_eq!(r.to_image_string(), "reg.io/img:v2");
    }

    #[test]
    fn image_bare_repository() {
        let r = ImageReference {
            repository: "reg.io/img".into(),
            tag: String::new(),
            digest: String::new(),
        };
        assert_eq!(r.to_image_string(), "reg.io/img");
    }

    #[test]
    fn serde_roundtrip() {
        let r = ImageReference {
            repository: "r".into(),
            tag: "t".into(),
            digest: String::new(),
        };
        let s = serde_json::to_string(&r).unwrap();
        let r2: ImageReference = serde_json::from_str(&s).unwrap();
        assert_eq!(r, r2);
    }
}
