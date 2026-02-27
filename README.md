# Inference Budget Controller

A Kubernetes operator that provides memory-aware scaling and admission control for LLM inference workloads. It coordinates multiple large language models competing for limited GPU memory resources, providing immediate feedback when models cannot be scheduled.

## Quick Start

### Install

```bash
# Clone and deploy
git clone https://github.com/cecil-the-coder/inference-budget-controller.git
cd inference-budget-controller

# Deploy using kustomize
kubectl apply -k config/default
```

### Create Your First Model

```yaml
apiVersion: inference.eh-ops.io/v1alpha1
kind: InferenceModel
metadata:
  name: llama-2-7b
spec:
  modelName: llama-2-7b
  memory: 16Gi
  backendUrl: http://vllm-server:8000
  nodeSelector:
    inference-pool: gpu-nodes
  cooldownPeriod: 10m
```

```bash
kubectl apply -f my-model.yaml
```

### Verify

```bash
kubectl get inferencemodels
# NAME          READY   REPLICAS   MEMORY   AGE
# llama-2-7b    true    1          16Gi     1m
```

### Documentation

- [Getting Started](./docs/getting-started.md) - Full installation and usage guide
- [CRD Reference](./docs/crd-reference.md) - Complete field documentation
- [Architecture](./docs/architecture.md) - How components work together
- [Admission Control](./docs/admission-control.md) - 429 responses and retry strategies
- [Observability](./docs/observability.md) - Metrics, logging, and alerting
- [Examples](./docs/examples/) - Sample configurations

---

## Problem Statement

The current inference stack (LiteLLM + KubeElasti) lacks:

1. **Memory awareness** - Doesn't know how much memory each model consumes
2. **Cross-model coordination** - Models scale independently, competing for shared memory
3. **Admission control** - Requests queue indefinitely when resources are insufficient
4. **Fast feedback** - No 429 response when a model can't be scheduled due to memory pressure

Example failure mode:
```
1. Model A (80GB) is running, serving requests
2. Model A becomes idle but hasn't scaled down yet (cooldownPeriod)
3. Request arrives for Model B (60GB)
4. Only 48GB free, can't schedule Model B
5. Request queues for 600s, then times out
6. User has no idea why it failed
```

Desired behavior:
```
1. Model A (80GB) is running
2. Request arrives for Model B (60GB)
3. Controller checks: 128GB - 80GB = 48GB < 60GB → Can't fit
4. Return 429 immediately with details:
   {
     "error": "insufficient_memory",
     "requested": {"model": "model-b", "memory": "60Gi"},
     "available": "48Gi",
     "blocking": [{"model": "model-a", "memory": "80Gi", "idle_for": "120s"}]
   }
```

## Architecture

```
                                    ┌─────────────────────────────────┐
                                    │         Kubernetes API          │
                                    └─────────────────────────────────┘
                                                  ▲
                                                  │ Watches/Manages
                                                  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                      inference-budget-controller                        │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────────────┐ │
│  │  CRD Controller │  │  Budget Tracker │  │     API Proxy           │ │
│  │                 │  │                 │  │  (OpenAI-compatible)    │ │
│  │ - Reconcile     │  │ - Track memory  │  │                         │ │
│  │   InferenceModel│  │ - Check capacity│  │ - Admission control     │ │
│  │ - Manage Deps   │  │ - Node budgets  │  │ - Route to backends     │ │
│  │ - Scale 0↔1     │  │                 │  │ - Return 429 if full    │ │
│  └────────┬────────┘  └────────┬────────┘  └────────────┬────────────┘ │
│           │                    │                        │              │
│           └────────────────────┴────────────────────────┘              │
│                               shared state                              │
└─────────────────────────────────────────────────────────────────────────┘
                                        │
                    ┌───────────────────┼───────────────────┐
                    ▼                   ▼                   ▼
            ┌──────────────┐    ┌──────────────┐    ┌──────────────┐
            │  Deployment  │    │  Deployment  │    │  Deployment  │
            │  model-a     │    │  model-b     │    │  model-c     │
            │  (replicas:0)│    │  (replicas:1)│    │  (replicas:0)│
            └──────────────┘    └──────────────┘    └──────────────┘
                    │                   │                   │
                    └───────────────────┴───────────────────┘
                                        │
                                        ▼
                                ┌──────────────┐
                                │  Node Pool   │
                                │  (e.g. shadow)│
                                │  128GB RAM   │
                                └──────────────┘
```

## Components

### 1. InferenceModel CRD

Custom Resource Definition that declares an inference model with its memory requirement.

### 2. Controller

Watches InferenceModel CRDs and manages underlying Deployments.

### 3. Budget Tracker

In-memory tracker of memory allocation per node.

### 4. API Proxy

OpenAI-compatible API that handles admission control and routing.

## Key Design Decisions

### Single Controller Instance

The controller must run as a single replica to maintain accurate memory budget state. Use leader election for HA.

### In-Memory State

Budget tracking is in-memory (not in CRD status) for performance. On restart, controller rebuilds state by examining running Deployments.

### No Persistent Queue

If a model can't be scheduled, return 429 immediately. Client is responsible for retry with backoff.

### Per-Node Memory Budgets

Each node has a fixed memory capacity. Models specify which node they run on via nodeSelector.

### Conservative Budgeting with Usage Observability

The controller takes a **measure and suggest** approach to memory management:

1. **Conservative by default** - Uses declared memory limits for budgeting decisions (never overcommit based on observed usage)
2. **Observability** - Tracks actual peak memory usage per model
3. **Recommendations** - Surfaces discrepancies between declared and actual usage to help users right-size allocations

#### Feedback Mechanisms

| Mechanism | Purpose |
|-----------|---------|
| **Logs** | Periodic hints when declared >> actual |
| **CRD Status** | Exposes observed metrics and recommendations |
| **Prometheus Metrics** | Dashboards and alerting on utilization |
| **API Endpoint** | Programmatic access to usage data (`GET /models/{name}/metrics`) |

#### Example Log Output

```
INFO  model-a declared 80Gi but observed peak is 75Gi (6% overprovisioned)
INFO  model-b declared 60Gi but observed peak is 58Gi (3% overprovisioned)
```

#### Example CRD Status

```yaml
status:
  observedPeakMemory: 75Gi
  declaredMemory: 80Gi
  utilizationPercent: 93
  recommendation: "Consider reducing memory to 78Gi for better packing"
  lastObservation: "2024-01-15T10:30:00Z"
```

#### Prometheus Metrics

```
# HELP inference_model_memory_declared_bytes Memory declared in the CRD
# TYPE inference_model_memory_declared_bytes gauge
inference_model_memory_declared_bytes{model="model-a"} 85899345920

# HELP inference_model_memory_observed_peak_bytes Peak memory observed at runtime
# TYPE inference_model_memory_observed_peak_bytes gauge
inference_model_memory_observed_peak_bytes{model="model-a"} 80530636800

# HELP inference_model_memory_utilization_ratio Ratio of observed to declared memory (0-1)
# TYPE inference_model_memory_utilization_ratio gauge
inference_model_memory_utilization_ratio{model="model-a"} 0.94
```

#### Key Principle

**Never auto-adjust** - The controller only observes and suggests. Users must explicitly update the CRD to change memory allocations. This keeps the system predictable and prevents OOM risks from automatic overcommitment.

## Comparison with Current Stack

| Feature | LiteLLM + KubeElasti | inference-budget-controller |
|---------|---------------------|----------------------------|
| Memory awareness | ❌ | ✅ |
| Admission control | ❌ | ✅ (429 responses) |
| Cross-model coordination | ❌ | ✅ |
| Scale to zero | ✅ | ✅ |
| Cold start handling | ✅ (queue) | ✅ (wait + progress) |
| Dependencies | 2 systems | 1 controller |
| Configuration | Split across 3 places | Single CRD per model |

## Project Structure

```
inference-budget-controller/
├── README.md                           # This file
├── DESIGN.md                           # Detailed design document
├── api/v1alpha1/
│   ├── inferencemodel_types.go         # CRD type definitions
│   ├── groupversion_info.go            # API group version
│   └── zz_generated.deepcopy.go        # Generated deepcopy functions
├── cmd/
│   └── main.go                         # Entry point
├── internal/
│   ├── controller/
│   │   ├── inferencemodel_controller.go    # Reconciliation loop
│   │   └── suite_test.go                   # Controller tests
│   ├── proxy/
│   │   ├── server.go                   # HTTP proxy server
│   │   ├── handler.go                  # Request handler
│   │   └── openai.go                   # OpenAI API types
│   └── budget/
│       ├── tracker.go                  # Memory budget tracker
│       └── tracker_test.go             # Unit tests
├── config/
│   ├── crd/
│   │   └── bases/
│   │       └── inference.eh-ops.io_inferencemodels.yaml
│   ├── rbac/
│   │   ├── role.yaml
│   │   ├── role_binding.yaml
│   │   └── service_account.yaml
│   └── manager/
│       └── manager.yaml                # Controller deployment
├── Dockerfile
├── go.mod
└── go.sum
```
