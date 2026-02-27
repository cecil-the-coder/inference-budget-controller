# Architecture

This document describes the architecture and components of the Inference Budget Controller.

## System Overview

The Inference Budget Controller is a Kubernetes operator that provides memory-aware scaling and admission control for LLM inference workloads. It solves the problem of multiple large language models competing for limited GPU memory resources.

```
+--------------------------------------------------------------------------+
|                           User / Client Application                       |
+--------------------------------------------------------------------------+
                                    |
                                    | HTTP Request (OpenAI API)
                                    v
+--------------------------------------------------------------------------+
|                           API Proxy Server                                |
|  +-------------------------------------------------------------------+   |
|  |  OpenAI-Compatible Endpoints                                      |   |
|  |  - POST /v1/chat/completions                                      |   |
|  |  - POST /v1/completions                                           |   |
|  |  - GET /v1/models                                                 |   |
|  +-------------------------------------------------------------------+   |
|                                    |                                      |
|  +-------------------------------------------------------------------+   |
|  |  Admission Control                                                |   |
|  |  - Check if model exists                                          |   |
|  |  - Check if model is ready                                        |   |
|  |  - Check memory budget availability                               |   |
|  |  - Return 429 if insufficient memory                              |   |
|  +-------------------------------------------------------------------+   |
|                                    |                                      |
|  +-------------------------------------------------------------------+   |
|  |  Request Forwarding                                               |   |
|  |  - Stream responses (SSE)                                         |   |
|  |  - Track request timestamps                                       |   |
|  +-------------------------------------------------------------------+   |
+--------------------------------------------------------------------------+
         |                           |                           |
         | Query/Update              | Query                     | Update
         v                           v                           v
+------------------+    +------------------------+    +-----------------------+
| Kubernetes API   |    |    Budget Tracker      |    |    Idle Tracker       |
|                  |    |    (In-Memory)         |    |    (In-Memory)        |
| - InferenceModel |    |                        |    |                       |
|   CRDs           |    | - Node budgets         |    | - Last request time   |
| - Deployments    |    | - Allocations          |    |   per model           |
| - Events         |    | - Utilization data     |    | - Idle duration       |
+------------------+    +------------------------+    +-----------------------+
         ^
         | Watch/Reconcile
         |
+--------------------------------------------------------------------------+
|                        CRD Controller (Manager)                           |
|  +-------------------------------------------------------------------+   |
|  |  Reconciliation Loop                                              |   |
|  |  - Watch InferenceModel CRDs                                      |   |
|  |  - Manage Deployments                                             |   |
|  |  - Scale to zero on idle                                          |   |
|  |  - Scale up on demand                                             |   |
|  |  - Update status conditions                                       |   |
|  +-------------------------------------------------------------------+   |
|  +-------------------------------------------------------------------+   |
|  |  Memory Budget Management                                         |   |
|  |  - Allocate on deployment creation                                |   |
|  |  - Release on scale-to-zero                                       |   |
|  |  - Release on deletion                                            |   |
|  +-------------------------------------------------------------------+   |
|  +-------------------------------------------------------------------+   |
|  |  Metrics Collection                                               |   |
|  |  - Export Prometheus metrics                                      |   |
|  |  - Track utilization                                              |   |
|  +-------------------------------------------------------------------+   |
+--------------------------------------------------------------------------+
                                    |
                                    | Creates/Manages
                                    v
+--------------------------------------------------------------------------+
|                            Managed Deployments                            |
|  +------------------+    +------------------+    +------------------+    |
|  |  Deployment A    |    |  Deployment B    |    |  Deployment C    |    |
|  |  (replicas: 0)   |    |  (replicas: 1)   |    |  (replicas: 0)   |    |
|  |  Memory: 80Gi    |    |  Memory: 48Gi    |    |  Memory: 16Gi    |    |
|  +------------------+    +------------------+    +------------------+    |
+--------------------------------------------------------------------------+
                                    |
                                    | Scheduled on
                                    v
+--------------------------------------------------------------------------+
|                              Node Pool                                    |
|  +-------------------------------------------------------------------+   |
|  |  GPU Node (128GB VRAM)                                            |   |
|  |  - Total Budget: 128Gi                                            |   |
|  |  - Allocated: 48Gi (Model B)                                      |   |
|  |  - Available: 80Gi                                                |   |
|  +-------------------------------------------------------------------+   |
+--------------------------------------------------------------------------+
```

## Components

### 1. InferenceModel CRD

The Custom Resource Definition that declares an inference model:

- **modelName**: Identifier used in API requests
- **memory**: Memory requirement for budget calculation
- **backendUrl**: Where to forward requests
- **nodeSelector**: Which node pool to use
- **cooldownPeriod**: Idle time before scale-down
- **resources**: Container resource requirements

### 2. CRD Controller

The main reconciliation loop that manages InferenceModel resources:

**Responsibilities:**
- Watch for InferenceModel create/update/delete events
- Create and manage underlying Deployments
- Handle scale-to-zero based on idle time
- Handle scale-up when requests arrive
- Update status conditions
- Manage finalizers for cleanup
- Emit Kubernetes events

**Reconciliation Flow:**

```
Reconcile(InferenceModel)
        |
        v
+-------------------+
| Check deletion    |
| timestamp         |
+-------------------+
        |
        | Not deleting
        v
+-------------------+
| Ensure finalizer  |
| is present        |
+-------------------+
        |
        v
+-------------------+
| Check if          |
| Deployment exists |
+-------------------+
        |
        +---> No --->+-------------------+
        |            | Check memory      |
        |            | budget            |
        |            +-------------------+
        |                    |
        |                    +---> Available ---> Create Deployment
        |                    |
        |                    +---> Not Available ---> Requeue
        |
        v
+-------------------+
| Check idle time   |
| for scale-to-zero |
+-------------------+
        |
        | Idle > cooldown
        v
+-------------------+
| Scale to zero     |
| Release budget    |
+-------------------+
        |
        v
+-------------------+
| Update status     |
| conditions        |
+-------------------+
        |
        v
+-------------------+
| Update metrics    |
+-------------------+
```

### 3. Budget Tracker

In-memory component that tracks memory allocations:

**Data Structures:**
```go
type Tracker struct {
    nodeBudgets   map[string]*NodeBudget    // Per-node-pool budgets
    allocations   map[string]*ModelAllocation // Model -> allocation
    usageTracking map[string]*modelUsage      // Observed peak memory
}

type NodeBudget struct {
    Total     *resource.Quantity  // Total capacity
    Allocated *resource.Quantity  // Currently allocated
}

type ModelAllocation struct {
    Name         string
    Namespace    string
    Memory       *resource.Quantity
    NodeSelector map[string]string
}
```

**Key Operations:**
- `CanAllocate(name, namespace, memory, nodeSelector)` - Check if memory is available
- `Allocate(...)` - Reserve memory for a model
- `ReleaseModel(name, namespace)` - Free memory allocation
- `GetAvailableMemory(nodeSelector)` - Query available capacity
- `GetBlockingModels(nodeSelector, requestedMemory)` - Find models blocking allocation
- `RecordUsage(name, namespace, observed)` - Track peak memory usage
- `GetUtilization(...)` - Get utilization metrics and recommendations

### 4. API Proxy

HTTP server that provides OpenAI-compatible API endpoints:

**Endpoints:**
- `POST /v1/chat/completions` - Chat completion requests
- `POST /v1/completions` - Text completion requests
- `GET /v1/models` - List available models
- `GET /healthz` - Health check
- `GET /readyz` - Readiness check

**Request Flow:**

```
Client Request
      |
      v
+----------------------+
| Parse request body   |
| Extract model name   |
+----------------------+
      |
      v
+----------------------+
| Lookup InferenceModel|
| by model name        |
+----------------------+
      |
      +---> Not found ---> 404 Model Not Found
      |
      v
+----------------------+
| Check if model ready |
+----------------------+
      |
      +---> Ready ---> Forward to backend
      |
      v
+----------------------+
| Check memory budget  |
| for scale-up         |
+----------------------+
      |
      +---> Insufficient ---> 429 with details
      |
      v
+----------------------+
| Allocate budget      |
| Scale up deployment  |
+----------------------+
      |
      v
+----------------------+
| Wait for readiness   |
| (with timeout)       |
+----------------------+
      |
      +---> Timeout ---> 503 Model Not Ready
      |
      v
+----------------------+
| Forward request to   |
| backend              |
+----------------------+
      |
      v
+----------------------+
| Stream response      |
| Record request time  |
+----------------------+
```

### 5. Idle Tracker

Tracks when models were last accessed:

```go
type IdleTracker struct {
    lastRequest map[string]time.Time // namespace/name -> last request time
}
```

**Operations:**
- `RecordRequest(namespace, name)` - Update last request timestamp
- `GetIdleDuration(namespace, name)` - How long since last request
- `GetIdleSince(namespace, name)` - When was the last request

### 6. Metrics Collector

Prometheus metrics exporter:

**Model Metrics:**
- `inference_model_memory_declared_bytes` - Declared memory allocation
- `inference_model_memory_observed_peak_bytes` - Peak observed memory
- `inference_model_memory_utilization_ratio` - Utilization ratio (0-1)
- `inference_model_ready` - Ready status (1 or 0)
- `inference_model_replicas` - Current replica count

**Budget Metrics:**
- `inference_budget_total_bytes` - Total budget per node pool
- `inference_budget_allocated_bytes` - Allocated memory per node pool
- `inference_budget_available_bytes` - Available memory per node pool

## Component Interaction

### Request Path (Happy Path)

```
1. Client sends POST /v1/chat/completions to Proxy
2. Proxy extracts model name from request
3. Proxy queries Kubernetes API for InferenceModel CRD
4. Proxy checks if model deployment is ready
5. If ready, Proxy forwards request to backendUrl
6. Proxy streams response back to client
7. Proxy records request timestamp in IdleTracker
```

### Request Path (Model Scaled to Zero)

```
1. Client sends POST /v1/chat/completions to Proxy
2. Proxy extracts model name from request
3. Proxy queries Kubernetes API for InferenceModel CRD
4. Proxy sees model is not ready (replicas: 0)
5. Proxy queries Budget Tracker for memory availability
6. If available:
   a. Proxy allocates budget in Tracker
   b. Proxy scales up Deployment
   c. Proxy waits for readiness (polls with delay)
   d. Proxy forwards request to backend
7. If not available:
   a. Proxy queries Tracker for blocking models
   b. Proxy returns 429 with blocking details
```

### Scale-to-Zero Flow

```
1. Controller reconciliation loop runs (every 1 minute)
2. For each InferenceModel with running replicas:
   a. Check IdleTracker for last request time
   b. Calculate idle duration
   c. If idle > cooldownPeriod:
      - Scale Deployment to 0 replicas
      - Release memory budget in Tracker
      - Update status conditions
      - Emit Kubernetes event
```

## Data Flow Diagram

```
                    +-------------------+
                    |   Client Request  |
                    +-------------------+
                            |
                            v
+---------------------------------------------------------------+
|                        API Proxy                               |
|  +-------------+  +-------------+  +------------------------+ |
|  |   Handler   |->|  Admission |->|    Request Forwarder   | |
|  +-------------+  |  Control    |  +------------------------+ |
|                   +-------------+              |               |
+---------------------------------------------------------------+
         |                |                       |
         |                v                       v
         |    +-------------------+    +-------------------+
         |    |   Budget Tracker  |    |   Backend Server  |
         |    |   (Shared Memory) |    |   (vLLM, etc.)    |
         |    +-------------------+    +-------------------+
         |                |
         v                |
+---------------------------------------------------------------+
|                     CRD Controller                            |
|  +-------------+  +-------------+  +------------------------+ |
|  |  Reconciler |->| Deployment  |->|    Status Updater     | |
|  |             |  |  Manager    |  |                       | |
|  +-------------+  +-------------+  +------------------------+ |
+---------------------------------------------------------------+
         |
         v
+---------------------------------------------------------------+
|                     Kubernetes API                             |
|  +-------------+  +-------------+  +------------------------+ |
|  |InferenceModel| | Deployments |  |       Events          | |
|  |    CRDs     |  |             |  |                       | |
|  +-------------+  +-------------+  +------------------------+ |
+---------------------------------------------------------------+
```

## High Availability Considerations

### Single Replica Requirement

The controller must run as a single replica because:
- Budget tracking is in-memory
- Multiple instances would have inconsistent state
- Leader election is used for HA failover

### Leader Election

When enabled, multiple controller replicas can run:
- Only the leader performs reconciliation
- Followers wait for leadership
- On leader failure, a follower takes over

### State Recovery

On controller restart:
1. Controller lists all InferenceModels
2. For each model with running replicas:
   - Re-allocate memory in Budget Tracker
   - Initialize Idle Tracker state
3. Resume normal operation

## Performance Characteristics

| Metric | Value | Notes |
|--------|-------|-------|
| Reconciliation interval | 30 seconds | Default for status updates |
| Idle check interval | 1 minute | For scale-to-zero decisions |
| Scale-up timeout | 5 minutes | Max wait for model readiness |
| Ready check delay | 2 seconds | Poll interval during scale-up |
| HTTP timeout | 10 minutes | For long inference requests |
| Proxy write timeout | 600 seconds | For streaming responses |
