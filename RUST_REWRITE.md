# Fox Operator — Rust Rewrite

Single-binary Rust replacement for the Go `inference-budget-controller` + external llama-server backend.
The binary IS the inference engine: it reconciles Kubernetes CRDs, downloads models, loads them in-process
via llama.cpp FFI, and serves the OpenAI/Ollama-compatible HTTP API — no sidecar pods.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Cargo Workspace](#2-cargo-workspace)
3. [Crate Reference](#3-crate-reference)
4. [Key Design Decisions](#4-key-design-decisions)
5. [Risk Register](#5-risk-register)
6. [Phased Implementation Plan](#6-phased-implementation-plan)
7. [Coexistence Strategy](#7-coexistence-strategy)
8. [Decision Points](#8-decision-points)
9. [Fastest Path to Validation](#9-fastest-path-to-validation)

---

## 1. Architecture Overview

### Before (current)

```
controller pod (Go)
  ├── reconciles InferenceModel CRDs
  ├── creates/deletes backend pods (llama-server)
  ├── tracks GTT budget
  └── reverse-proxies /v1/chat/completions → backend pod
```

### After (target)

```
fox-operator pod (Rust)
  ├── reconciles InferenceModel CRDs  [controller crate]
  ├── downloads models from HF/XetHub  [model-download crate]
  ├── loads models in-process via llama.cpp FFI  [fox-engine crate]
  ├── tracks GTT budget in-process  [budget crate]
  └── serves /v1/chat/completions directly  [fox-api crate]
```

**No backend pods. No reverse proxy. The DaemonSet pod IS the serving engine.**

Scale-to-zero = `llama_model_free()` — weights evicted from GTT, pod keeps running.
Scale-from-zero = `llama_model_load()` — weights loaded back, serving resumes.

### Hardware target

- Node: `shadow` — AMD Ryzen AI 385+ / Radeon 8060S (gfx1151, RDNA 3.5)
- GTT: 120 GB (system RAM used as VRAM via Graphics Translation Table)
- GPU backend: Vulkan (`GGML_VULKAN=ON`, RADV ICD)

---

## 2. Cargo Workspace

```
fox-operator/                        # Cargo workspace root
├── Cargo.toml                       # workspace members + shared deps + feature flags
├── Cargo.lock
├── Dockerfile                       # 4-stage build: sys-deps → cmake-build → rust-build → runtime
├── Makefile
├── crates/
│   ├── llama-sys/                   # build.rs (cmake llama.cpp) + bindgen FFI
│   ├── fox-engine/                  # InferenceEngine, Scheduler, KVCacheManager, ModelRegistry
│   ├── fox-api/                     # axum HTTP server: OpenAI + Ollama routes, SSE streaming
│   ├── k8s-crd/                     # #[derive(CustomResource)] for InferenceModel + InferenceBackend
│   ├── budget/                      # BudgetTracker: per-node GTT accounting
│   ├── model-download/              # DownloadManager: HuggingFace Hub, XetHub XORB, chunked HTTP
│   ├── controller/                  # kube_runtime reconciler, finalizer, idle eviction loop
│   ├── operator-metrics/            # Prometheus GaugeVec/CounterVec for operator plane
│   └── fox-operator/                # binary: main.rs, CLI, startup wiring
```

### Crate dependency graph

```
llama-sys
    └── fox-engine
            └── fox-api
                    └── fox-operator (binary)

k8s-crd
    ├── budget
    ├── model-download
    ├── operator-metrics
    └── controller
            └── fox-operator (binary)
```

`fox-operator` depends on all crates and wires them together at startup.

### Feature flags

| Flag | Effect |
|---|---|
| `vulkan` | `GGML_VULKAN=ON`; links `libggml-vulkan` + `libvulkan`. **Default for shadow.** |
| `cuda` | `GGML_CUDA=ON`; links CUDA runtime + cuBLAS |
| `metal` | `GGML_METAL=ON`; links Metal framework (macOS) |
| `cpu-only` | All GPU backends off; CI and CPU-only nodes |

Build for shadow: `cargo build --release --features vulkan`

---

## 3. Crate Reference

### `llama-sys`

Raw C FFI bindings to llama.cpp. The only crate that touches the C++ build system.

- `build.rs`: cmake configure (`GGML_VULKAN=ON` / `GGML_CUDA=ON` / `GGML_METAL=ON`), bindgen generation
- `LLAMA_PREBUILT_DIR` env var: skip cmake, use pre-built `.a` from Docker stage (decouples cmake and rust build layers)
- Vulkan additions vs fox's original `build.rs`:
  ```rust
  } else if env::var("CARGO_FEATURE_VULKAN").is_ok() {
      cmake_config.define("GGML_VULKAN", "ON");
      if let Ok(sdk) = env::var("VULKAN_SDK") {
          cmake_config.define("VULKAN_SDK", &sdk);
      }
  }
  // link block:
  println!("cargo:rustc-link-lib=static=ggml-vulkan");
  println!("cargo:rustc-link-lib=dylib=vulkan");  // loader, never static
  ```
- `src/lib.rs`: `include!(concat!(env!("OUT_DIR"), "/llama_bindings.rs"))`

### `fox-engine`

Safe Rust wrappers over llama.cpp + continuous-batch serving logic. No HTTP, no k8s.

Modules transplanted from fox:
- `engine/mod.rs` — `InferenceEngine`, output filtering, think-block suppression
- `engine/model.rs` — `Model` trait, `LlamaCppModel`, `ModelConfig`
- `kv_cache/mod.rs` — `KVCacheManager`, `PageTable`, `compute_block_hash`, `prompt_block_hashes`
- `scheduler/mod.rs` + `scheduler/batch.rs` — `Scheduler`, `InferenceRequest`, `SamplingParams`, `Token`, `StopReason`
- `model_registry.rs` — `ModelRegistry`, `ModelHandle`, `LoadParams`
- `metrics.rs` — `EngineMetrics` (Prometheus)

**Key change vs fox**: `gpu_memory_bytes` moves from a global `RegistryConfig` to a per-call `LoadParams`:

```rust
pub struct LoadParams {
    pub gpu_memory_bytes: usize,       // GTT_budget - weight_bytes (from controller)
    pub gpu_memory_fraction: f32,      // 0.95 typically
    pub max_context_len: usize,
    pub max_batch_size: usize,
    pub block_size: usize,             // 16 tokens/block default
    pub n_gpu_layers: i32,             // -1 = all layers on GPU
}

impl ModelRegistry {
    pub async fn get_or_load(&self, name: &str, params: LoadParams) -> Result<Arc<ModelHandle>>;
    pub fn unload(&self, name: &str) -> bool;
    pub fn list_loaded(&self) -> Vec<(String, Arc<ModelHandle>)>;
    pub fn is_loaded(&self, name: &str) -> bool;
}
```

**Budget handoff chain**:
```
controller reconcile()
  → LoadParams { gpu_memory_bytes: gtt_budget - weight_bytes }
  → ModelRegistry::get_or_load(name, params)
    → KVCacheManager::new(&config, params.gpu_memory_bytes, params.gpu_memory_fraction, block_size)
      → total_blocks = floor(gpu_memory_bytes × fraction / bytes_per_block)
```

**Scheduler fixes** required vs current fox code (identified during design):

| # | Fix | Priority |
|---|---|---|
| R-05 | `Drop for PrefixCacheEntry`: free KV blocks + return seq_id on LRU eviction | **P0 critical bug** |
| — | Add `Draining`, `Cancelled` states; `ModelUnloading`, `Cancelled` stop reasons | High |
| — | `begin_drain()` / `is_drained()` for scale-to-zero | High |
| — | Chunked prefill: `prefill_offset` + `prefill_chunk_size` (critical for 94-layer MoE) | High |
| — | Admission control accounts for prefix-cache-pinned blocks | High |
| — | Least-progress preemption instead of pure LIFO | Medium |

**MoE KV math** (Qwen3-235B on shadow):
```
bytes_per_block = 16 × 94 × 4 × 128 × 2 × 2 = ~2.94 MB/block
80 GB KV budget → ~27,887 blocks → ~446K tokens total capacity

BUT: Qwen3-235B at Q4_K_M ≈ 120 GB weights
→ 120 GB GTT - 120 GB weights = ~0 GB for KV

validate_memory_budget() enforces a minimum KV floor and rejects infeasible specs.
```

**spawn_blocking contract**: all `llama_decode()` calls run in `tokio::task::spawn_blocking`. The `run_loop` is async; only the FFI call blocks. No mutex is held across an `.await` point.

### `fox-api`

axum HTTP server. Replaces both fox's `src/api/` and the Go proxy's `internal/proxy/`.

**AppState**:
```rust
pub struct AppState {
    pub registry: Arc<ModelRegistry>,        // in-process inference (no proxy)
    pub download_manager: Arc<DownloadManager>,
    pub idle_tracker: IdleTracker,           // Arc<Mutex<HashMap<String, Instant>>>
    pub load_timeout: Duration,              // default 5 min (configurable)
    pub models_dir: PathBuf,
    pub system_prompt: Option<String>,
}
```

**Route table**:
```
POST /v1/chat/completions      → chat_completions (SSE or JSON)
POST /v1/completions           → completions
POST /v1/embeddings            → v1_embeddings
GET  /v1/models                → list_models_openai
POST /api/chat                 → ollama_chat
POST /api/generate             → ollama_generate
GET  /api/tags                 → ollama_tags
GET  /api/ps                   → ollama_ps
POST /api/show                 → ollama_show
DELETE /api/delete             → ollama_delete
POST /api/embed                → ollama_embed
POST /api/pull                 → ollama_pull (SSE progress stream)
GET  /health                   → health
GET  /healthz                  → liveness probe (k8s)
GET  /readyz                   → readiness probe (503 until ≥1 model loaded)
GET  /metrics                  → Prometheus text
```

**Scale-from-zero** (`ensure_loaded()`):
- `LoadGate`: one `Arc<Notify>` per model — prevents N concurrent requests each spawning a load
- First caller submits load task; all others `.await` the `Notify`
- Returns 503 with `Retry-After` if load times out (configurable, default 5 min)
- SSE keepalive comment every 10s while loading: `": loading model, estimated Xs"`

**SSE streaming fixes vs fox**:
- `role: "assistant"` in first chunk only (OpenAI spec)
- `data: [DONE]` sentinel after final chunk (clients hang without it)
- `yield_now()` between tokens for per-token flush

**IdleTracker**: `touch(&model_name)` called before each inference dispatch; `idle_duration()` read by controller's eviction loop. Shared via `Arc<Mutex<>>` between `AppState` and the controller `ControllerCtx`.

### `k8s-crd`

Rust CRD type definitions using `#[derive(CustomResource)]` from kube-rs.

```rust
#[kube(group = "inference.eh-ops.io", version = "v1alpha1",
       kind = "InferenceModel", namespaced,
       status = "InferenceModelStatus",
       printcolumn = r#"{"name":"Phase","type":"string","jsonPath":".status.phase"}"#, ...)]
pub struct InferenceModelSpec { ... }
```

**`Quantity` newtype** (wire-compatible with Kubernetes API):
```rust
#[derive(Serialize, Deserialize, JsonSchema)]
#[schemars(transparent)]
pub struct Quantity(pub String);

impl Quantity {
    pub fn as_bytes(&self) -> Result<u64>;  // "80Gi" → 85_899_345_920
}
```

**`InferenceBackend` simplified**: no more pod template fields (`InitContainers`, `Volumes`, `SecurityContext`).
New fields instead:
```rust
pub struct InferenceBackendSpec {
    pub engine: InferenceEngine,           // LlamaCpp | Vllm | Ollama | Custom(String)
    pub serving: Option<ServingConfig>,    // max_concurrent_requests, block_size, etc.
    pub node_selector: BTreeMap<String, String>,
}
```

`scheme.rs`: `install_crds(client)` (idempotent server-side apply) + `wait_for_crds(client)`.

### `budget`

In-memory GTT budget accounting. Async `RwLock` over plain `BudgetState`.

```rust
impl BudgetTracker {
    pub async fn can_allocate(&self, name, ns, bytes, pool_key) -> bool;
    pub async fn allocate(&self, name, ns, bytes, pool_key) -> bool;  // atomic check+commit
    pub async fn release(&self, name, ns);
    pub async fn sync_allocation(&self, name, ns, bytes, pool_key) -> bool;  // startup recovery
    pub async fn record_peak(&self, name, ns, observed_bytes);
    pub async fn utilisation(&self, name, ns, declared) -> (Bytes, Option<Bytes>, f64);
    pub async fn total_allocated_bytes(&self) -> u64;
}
```

Startup recovery (R-04 mitigation): on operator restart, `ModelRegistry::list_loaded()` → `sync_allocation()` for each loaded model, rehydrating `BudgetState` from running engine state before entering the reconcile loop.

### `model-download`

Async model downloads from HuggingFace Hub, XetHub XORB, and plain HTTP.

```rust
impl DownloadManager {
    pub fn start_download(&self, model_id, source, dest, hf_token) -> Result<()>;
    pub fn download_status(&self, model_id) -> DownloadStatus;
    pub fn is_downloading(&self, model_id) -> bool;
    pub fn cancel(&self, model_id);
    /// SSE progress stream for /api/pull
    pub fn progress_stream(&self, model_id) -> impl Stream<Item = DownloadProgress>;
}
```

Downloads bounded by a semaphore (`max_concurrent = 3`).

HuggingFace client: bearer auth, `list_repo_files()`, parallel range requests (adaptive chunk size, ~8 MB default), SHA-256 checksum verify.

XORB backend behind `DownloadBackend` trait → can disable with `--disable-xorb` or fallback to HF HTTP on any error. Token refresh on 401.

### `controller`

kube_runtime reconciler for `InferenceModel`.

**`ControllerCtx`**:
```rust
pub struct ControllerCtx {
    pub client: Client,
    pub registry: Arc<ModelRegistry>,
    pub scheduler: Arc<Scheduler>,
    pub budget: Arc<BudgetTracker>,
    pub idle_tracker: IdleTracker,     // shared with AppState
    pub metrics: Arc<OperatorMetrics>,
    pub max_memory_bytes: u64,
    pub idle_timeout: Option<Duration>,
    pub model_cache_dir: PathBuf,
    pub hf_token: Option<String>,
}
```

**Reconcile phases**:
1. **Finalizer** — via `kube_runtime::finalizer::finalizer()` (handles add/remove automatically)
2. **Download** — if `needs_download()`, call `DownloadManager::start_download()`, poll at 2s intervals
3. **Budget** — `parse_bytes(spec.resources.memory)` → `BudgetTracker::allocate()`, requeue 30s on exhaustion
4. **Load** — `ModelRegistry::get_or_load(name, LoadParams)` via `spawn_blocking`
5. **Idle check** — if `idle_duration > cooldown_period`, drain + unload + release budget
6. **Status patch** — `Api::patch_status` with JSON Merge Patch (status subresource only)

**Error → requeue mapping**:

| Error | Action |
|---|---|
| `InvalidSpec` | `await_change()` (user must fix spec) |
| `BudgetExhausted` | `requeue(30s)` |
| `DownloadInProgress` | `requeue(2s)` |
| `DownloadFailed` | `await_change()` |
| `ModelLoad` | `requeue(1s)` + exponential backoff |
| `StatusConflict` | `requeue(500ms)` |

**Status patch** — only touches status subresource:
```rust
StatusPatch::new(&ctx.client, &ns, &name)
    .phase("Running")
    .ready(true)
    .replicas(1)
    .condition(make_condition("Ready", "True", "Deployed", "...", generation))
    .apply().await?;
```

**IdleEvictionLoop**: background task polling every 30s. Handles `MemoryPressureOnly` eviction policy (evict idle models when node budget ≥ 90% full). Safety net for missed reconcile requeues after operator restart.

**Node-scope filtering** (R-10): reconciler filters events to `InferenceModel` objects whose `nodeSelector` matches the pod's own node (via downward API). Prevents two DaemonSet pods on different nodes fighting over the same object.

**Drain protocol**:
```rust
scheduler.begin_drain(&model_name);
tokio::time::timeout(Duration::from_secs(30), drain_complete).await;
// force-unload if timeout
registry.unload(&model_name);
budget.release(&name, &ns).await;
```

### `operator-metrics`

Prometheus metrics for the operator/controller plane (separate from engine-level metrics in `fox-engine`):

```
inference_budget_allocated_bytes        (model, namespace, node_selector)
inference_budget_observed_peak_bytes    (model, namespace)
inference_budget_utilization_ratio      (model, namespace)
inference_model_ready                   (model, namespace)
inference_model_replicas                (model, namespace)
inference_admission_requests_total      (result=allowed|denied)
inference_node_memory_total_bytes       (node_selector)
inference_node_memory_available_bytes   (node_selector)
inference_download_phase                (model, phase)
inference_download_bytes_total/done     (model)
```

### `fox-operator` (binary)

Startup sequence:
1. Parse CLI flags (`clap`)
2. Init tracing subscriber (JSON, `RUST_LOG`)
3. Build `kube::Client` from in-cluster config
4. Apply CRDs if `--install-crds`
5. Construct shared state: `BudgetTracker`, `DownloadManager`, `ModelRegistry`, `IdleTracker`, `OperatorMetrics`
6. Startup budget recovery: `list_loaded()` → `sync_allocation()` for each loaded model
7. Spawn background tasks: `CacheManager`, `IdleEvictionLoop`
8. Start axum HTTP server on `--inference-bind-address` (default `:9000`)
9. Start metrics HTTP server on `--metrics-bind-address` (default `:8080`)
10. Start `kube_runtime::Controller` for `InferenceModel`
11. Await `SIGTERM` → graceful drain → unload all → exit

Two modes: `fox-operator operator` (k8s DaemonSet) and `fox-operator serve` (standalone dev, no k8s).

---

## 4. Key Design Decisions

### Why one binary instead of controller + backend pod?

- **Exact budget accounting**: `llama_model_load()` happens in-process; the operator knows exactly how much GTT is consumed after load returns, not from an annotation guess.
- **Scale-to-zero is `llama_model_free()`**: no pod delete/create latency, no scheduling delay. GTT is returned in ~100ms.
- **No proxy hop**: `POST /v1/chat/completions` → scheduler → llama.cpp, zero network round-trips.
- **Shared idle tracker**: the HTTP layer and the reconciler share the same `Arc<Mutex<>>` — no IPC needed to know when a model was last used.

### Why Rust instead of staying Go?

- fox is already Rust + llama.cpp FFI; there is no idiomatic Go→llama.cpp FFI story
- CGo has thread pinning overhead per call; Rust FFI is zero-cost
- `kube-rs` + `axum` + `tokio` share the same async runtime — no inter-runtime marshalling
- A single Cargo workspace: CRD types, HTTP types, engine types are all shared at compile time with no code generation step

### Why keep fox's scheduler instead of building from scratch?

fox's scheduler (continuous batching, PagedAttention, prefix cache, preemption) is ~700 lines of tested Rust. The design has 8 identified bugs/gaps (see §3 fox-engine), but the core data structures are correct. Building from scratch would take longer and produce the same bugs at different code locations.

### Why Vulkan instead of ROCm/HIP?

- gfx1151 ships with Mesa RADV ICD pre-installed; no separate ROCm package required
- llama.cpp's Vulkan backend is less mature than HIP but has fewer system-level dependencies
- Container runtime: `libvulkan.so.1` + `mesa-vulkan-drivers` ≈ 30 MB; ROCm ≈ 3–4 GB
- If Vulkan latency is unacceptable, Decision Point D-01/D-02 governs the ROCm pivot

---

## 5. Risk Register

Risks scored: **L** = Likelihood (1–5), **I** = Impact (1–5), **Score** = L × I.

| ID | Risk | L | I | Score | Priority |
|---|---|---|---|---|---|
| R-05 | PrefixCacheEntry Drop bug: KV blocks leak on LRU eviction | 5 | 4 | 20 | **P0** |
| R-02 | GTT budget exhausted by weights before KV gets any space | 4 | 4 | 16 | **P0** |
| R-04 | Budget double-allocation on operator restart | 4 | 4 | 16 | **P0** |
| R-01 | llama.cpp FFI memory safety / UB on Vulkan decode path | 3 | 5 | 15 | P1 |
| R-09 | Scale-from-zero UX: 5-min hanging request with no feedback | 4 | 3 | 12 | P1 |
| R-03 | Vulkan device selection wrong on multi-ICD nodes | 3 | 4 | 12 | P1 |
| R-06 | cmake stage cache invalidation: 30-min rebuild on any llama.cpp change | 3 | 3 | 9 | P2 |
| R-07 | XetHub XORB wire format churn / auth token expiry | 3 | 3 | 9 | P2 |
| R-10 | DaemonSet multi-node reconcile conflict | 2 | 4 | 8 | P2 |
| R-08 | axum / kube_runtime version conflict on tower/hyper | 2 | 3 | 6 | P3 |

**Mitigations:**

**R-05**: `impl Drop for PrefixCacheEntry` → call `KVCacheManager::free_blocks()` + return `seq_id` to pool. Unit test: create 1000 entries, drop all, assert `free_block_count()` returns to initial value. Fix before Phase 2 integration testing.

**R-02**: `validate_memory_budget(total_gtt, weight_bytes, kv_budget, activation_overhead)` — errors with human-readable message if `weight_bytes > GTT * 0.90`. Enforce a minimum KV floor (1 GB or 256 blocks). Add admission webhook that rejects specs exceeding node capacity.

**R-04**: Startup recovery phase (before entering reconcile loop): `ModelRegistry::list_loaded()` → `BudgetTracker::sync_allocation()` for each loaded model. Integration test: kill operator mid-load, restart, verify budget is consistent.

**R-01**: `LlamaModel` holds `Arc<Inner>` preventing drop while decode in-flight. Run decode loop on a dedicated `std::thread` (not tokio blocking pool) with `LockOSThread` semantics. CI: `cargo +nightly test -Zsanitizer=address`.

**R-09**: Return `202 Accepted` with `Retry-After` header when model is loading. SSE keepalive comment every 10s: `": loading model, estimated Xs\n\n"`. Surface `loading_progress` in `/v1/models`.

**R-03**: Init container `vulkan-check` enumerates devices via `ggml_vk_list_devices()`, writes selected index to env var. Fail-fast if no GPU found. `FOX_VK_DEVICE_INDEX` env var for manual override.

**R-06**: Tag cmake layer by llama.cpp git SHA. Document upgrade procedure: bump SHA → push cmake image → bump Rust code.

**R-07**: `DownloadBackend` trait with `HfHttpBackend` and `XorbBackend`. `--disable-xorb` flag. Token refresh on 401.

**R-10**: Reconciler filters events by `nodeSelector` matching the pod's own node (downward API).

**R-08**: Pin exact versions in Cargo.toml at project start. Resolve dependency tree before writing application code (Phase 0 task).

---

## 6. Phased Implementation Plan

### Phase 0 — Skeleton & Toolchain (1–2 weeks)

**Done when:**
- `cargo build --workspace` passes on fresh clone with `FOX_SKIP_LLAMA=1`
- `cargo test --workspace` passes with stub tests
- CI check + test jobs complete in < 5 min
- 4-stage Docker build completes end-to-end
- No duplicate major versions of `tokio`, `hyper`, `tower` in dependency tree

**Tasks:** workspace init, all Cargo.tomls with pinned versions, empty crate stubs, `llama-sys/build.rs` with `LLAMA_PREBUILT_DIR` shortcut, 4-stage Dockerfile skeleton, GHA pipeline with FOX_SKIP_LLAMA=1.

---

### Phase 1 — llama-sys + llama-rs FFI Proof (2–3 weeks) ⚠️ Highest risk

**Done when:**
- `hello_llama` example loads a 7B GGUF, outputs 20 tokens on target GPU without crash
- ASAN reports zero errors
- Vulkan device selected correctly (logged)
- TTFT < 500 ms on 7B model at batch size 1

**Tasks:** full `build.rs` with Vulkan, minimal `llama_model_load` / `llama_decode` / `llama_get_logits_ith` bindings, `LlamaModel::Drop` guard, ASAN run in CI.

**Blocks all subsequent phases.** See Decision Point D-01/D-02.

---

### Phase 2 — fox-engine Core: Scheduler + KV Cache (2–3 weeks)

**Done when:**
- 10 concurrent requests scheduled, tokens match reference output
- KV block free-list returns to initial size after all requests complete (R-05 fix verified)
- `validate_memory_budget()` rejects infeasible loads
- `begin_drain()` + `is_drained()` work correctly

**Tasks:** port `kv_cache/mod.rs`, `scheduler/mod.rs`, `scheduler/batch.rs`; fix R-05; add `Draining`/`Cancelled` states; implement `begin_drain()`; implement chunked prefill; implement `validate_memory_budget()`.

---

### Phase 3 — ModelRegistry + BudgetTracker (1 week)

**Done when:**
- Load 3 models, budget accounting exact, no drift
- Operator restart recovers budget from running engines (R-04 verified)
- LRU eviction releases budget correctly

**Tasks:** `ModelRegistry` with `get_or_load(name, LoadParams)`, `BudgetTracker` with startup recovery, integration test simulating restart.

---

### Phase 4 — fox-api HTTP Server (1–2 weeks)

*(Can overlap with Phase 3 once ModelRegistry API is stable)*

**Done when:**
- `POST /v1/chat/completions` returns valid OpenAI SSE with real tokens
- Concurrent requests for unloaded model trigger exactly one load (LoadGate test)
- `/v1/models`, `/healthz`, `/readyz`, `/metrics` all return correctly
- IdleTracker shows non-zero `idle_duration` after 10s of no requests

**Tasks:** `AppState`, `LoadGate`, `ensure_loaded()`, all routes, SSE streaming with `[DONE]` + keepalive, `IdleTracker`.

---

### Phase 5 — model-download (1–2 weeks)

*(Can overlap with Phase 3)*

**Done when:**
- 4 GB GGUF downloads correctly from mock HF server with parallel range requests
- Progress SSE stream emits bytes/sec and ETA
- Semaphore limits concurrency correctly
- 401 triggers re-auth retry

**Tasks:** `DownloadManager`, `HfHttpBackend`, `XorbBackend` behind trait, `DownloadBackend` abstraction, progress stream, wiremock unit tests.

---

### Phase 6 — k8s-crd + Controller Reconciler (2–3 weeks)

**Done when:**
- Creating `InferenceModel` in kind cluster → operator downloads, loads, marks Ready
- Deleting `InferenceModel` → finalizer drains, unloads, releases budget, removes finalizer
- `BudgetExhausted` → correct 30s requeue + status condition
- After `cooldownPeriod` → model evicted, budget released
- Operator restart → budget state recovered (R-04)

**Tasks:** k8s-crd types, `install_crds()`, reconcile phases (download → budget → load → idle → status), error taxonomy, finalizer via `kube_runtime::finalizer`, node-scope filtering, `IdleEvictionLoop`, envtest integration tests.

---

### Phase 7 — Metrics (1 week)

*(Can overlap with Phase 6)*

**Done when:**
- All metrics from Go operator's Prometheus exposition present in Rust equivalent
- `inference_budget_allocated_bytes` and `inference_kv_utilization_ratio` accurate
- `/metrics` scrapeable without auth

---

### Phase 8 — Build, Container & DaemonSet (1 week)

**Done when:**
- `docker build` completes in < 40 min on warm cache, < 60 min cold
- Image boots on shadow-node, passes `vulkan-check` init container, serves test request
- Kind integration tests pass in CI

**Tasks:** finalize 4-stage Dockerfile, DaemonSet YAML with `/dev/dri` + `/models` hostPath mounts + render group + `vulkan-check` init container, `Makefile` with `shadow-build` target.

---

### Phase 9 — Cut-over & Go Operator Retirement (1–2 weeks)

1. Deploy Rust operator in shadow mode (observe events, no modifications to Go-managed resources) for one week
2. Verify metric parity
3. Migrate one non-critical model (relabel to `fox.inference/operator: rust`)
4. Gradually migrate all models
5. Scale Go operator to 0, remove after 72h bake

**Done when:** All `InferenceModel` objects reconciled by Rust operator; no budget drift in 72h; Go operator removed.

### Timeline summary

| Phase | Duration | Cumulative |
|---|---|---|
| 0 Skeleton | 1–2 wk | 2 wk |
| 1 FFI Proof | 2–3 wk | 5 wk |
| 2 Engine Core | 2–3 wk | 8 wk |
| 3 Registry + Budget | 1 wk | 9 wk |
| 4 HTTP API | 1–2 wk | 11 wk |
| 5 Download | 1–2 wk | 13 wk |
| 6 Controller | 2–3 wk | 16 wk |
| 7 Metrics | 1 wk | 17 wk |
| 8 Container | 1 wk | 18 wk |
| 9 Cut-over | 1–2 wk | 20 wk |

**~18–20 weeks** for 2–3 engineers. Phases 4 and 5 can overlap with 3; phases 6 and 7 can overlap once 3 is done.

---

## 7. Coexistence Strategy

The Go and Rust operators must not fight over the same `InferenceModel` objects during migration.

### Label partitioning

```yaml
metadata:
  labels:
    fox.inference/operator: rust   # or: go
```

Each operator's informer uses a `LabelSelector` filter. Objects without the label default to Go during transition. Migration = relabeling objects one at a time.

### Non-conflicting finalizers

- Go operator: `inference.eh-ops.io/cleanup`
- Rust operator: `fox.inference.io/cleanup`

### Non-conflicting status conditions

- Go operator writes condition type `Ready`
- Rust operator additionally writes `FoxRustReady` during parallel operation

### Migration sequence

1. Deploy Rust operator on shadow-nodes only (new node pool, no overlap)
2. Create new `InferenceModel` objects with `fox.inference/operator: rust`
3. Validate for 1–2 weeks
4. Relabel existing objects one at a time
5. Scale Go operator to 0 once all objects migrated
6. Remove Go operator after 72h bake period

---

## 8. Decision Points

### D-01 — Vulkan/RDNA4 path not viable in llama.cpp

**Trigger:** Phase 1 `hello_llama` crashes or produces numerically incorrect output on gfx1151.

**Pivot:** Add `GGML_HIP=ON` to `build.rs`, add ROCm runtime to Dockerfile. Vulkan becomes secondary. Timeline: +2 weeks.

### D-02 — Vulkan latency too high for interactive use

**Trigger:** TTFT > 500 ms on 7B model at batch size 1 on shadow.

**Pivot:** Investigate Flash Attention for the Vulkan KV path, or F16 KV caching. If insufficient, evaluate HIP attention kernel integration. Timeline: +3–4 weeks.

### D-03 — kube_runtime node-scope filtering insufficient

**Trigger:** Phase 6 reveals races when objects are relabeled between nodes.

**Pivot:** Distributed `Lease` lock per `InferenceModel` object — each fox-operator pod claims a lease before reconciling. Timeline: +1 week.

### D-04 — GTT ceiling infeasible for target models

**Trigger:** `validate_memory_budget()` consistently rejects all models above 13B on 120 GB GTT.

**Pivot:** Enable llama.cpp `mmap`-based weight loading (weights paged in from disk on demand, reducing GTT pressure at cost of higher cold-prompt latency). Or reduce `n_gpu_layers` to partial GPU offload. May require hardware changes.

### D-05 — XetHub XORB format changes incompatibly

**Trigger:** Phase 5 tests fail against new XetHub endpoint.

**Pivot:** Drop XORB, use HF HTTP chunked download for all sources. `DownloadBackend` trait makes this a 1-day change. No timeline impact.

### D-06 — axum + kube dependency conflict unresolvable

**Trigger:** Phase 0 produces irreconcilable `hyper` version conflict.

**Pivot:** Isolate HTTP server and k8s controller in separate Tokio runtimes via `[patch]`. Fallback: split into two processes over Unix socket. Timeline: +1 week.

---

## 9. Fastest Path to Validation

The highest-uncertainty question: **Can we load a GGUF model via llama.cpp's Vulkan backend on gfx1151 and get correct token output?**

### 3-day spike (run before committing to Phase 0)

**Day 1:** cmake llama.cpp in Docker with `GGML_VULKAN=ON`. Confirm `libllama.a` + `libggml-vulkan.a` produced.

**Day 2:** Minimal `llama-sys` crate with just `llama_model_load_from_file`, `llama_new_context_with_model`, `llama_decode`. Load a 1.5B model (Qwen2.5-1.5B-Q4_K_M, ~1 GB), run 20 tokens.

**Day 3:** Verify output against llama.cpp CLI reference. Enable Vulkan validation layers. Measure TTFT.

**Decision gate:**
- Correct + TTFT < 500 ms → proceed with full Phase 0 plan
- Correct + TTFT > 500 ms → open D-02; continue Phase 0 while investigating
- Incorrect or crash → open D-01; evaluate ROCm pivot

### What can be built in parallel during the spike

All pure-Rust work has zero dependency on the FFI result:

| Work | Phase | FFI dependency |
|---|---|---|
| Cargo workspace skeleton | 0 | None |
| k8s-crd type definitions | 6 | None |
| BudgetTracker unit tests | 3 | None |
| KVCacheManager (pure Rust) | 2 | None |
| Scheduler logic (with mock tokens) | 2 | None |
| fox-api route skeleton | 4 | None |
| HuggingFace HTTP client | 5 | None |
| CI pipeline | 0 | None |

Only `LlamaModel` and `InferenceEngine::run_loop()` hard-block on the spike result.

---

*Design produced by 10 sequential design agents, synthesized 2026-03-24.*
*Individual crate design documents available in project memory.*
