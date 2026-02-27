# Inference Proxy Design

This document describes the architecture for integrating an inference proxy into the inference-budget-controller, enabling true serverless inference where deployments are created on-demand and cleaned up when idle.

## Current Architecture

```
Client → External Proxy → InferenceBackend Pods (managed by controller)
                           ↑
                    Deployments always exist
                    (scaled to 0 when idle)
```

**Current behavior:**
- Controller watches InferenceModel CRDs
- Creates Deployments that persist even at 0 replicas
- External proxy handles traffic routing
- Scale-up triggered by external mechanism

**Limitations:**
- Deployments consume etcd storage even when unused
- Clutters deployment lists with "empty" deployments
- Requires external proxy component

## Proposed Architecture

```
Client → Controller (Proxy) → InferenceBackend Pods
              ↓
        On-demand deployment creation
        Cleanup when idle
```

**New behavior:**
- Controller acts as the inference proxy
- Deployments only exist when serving requests
- Zero footprint when idle
- Single component for both control plane and data plane

## Components

### 1. HTTP Proxy Server

Runs within the controller pod as a separate HTTP server:

```
Routes:
  POST /v1/chat/completions     - Chat completions (OpenAI compatible)
  POST /v1/completions          - Text completions
  POST /v1/embeddings           - Text embeddings
  POST /v1/audio/transcriptions - Audio transcription (Whisper)
  GET  /v1/models               - List available models
  GET  /health                  - Health check
```

### 2. Request Handler Flow

```
Request arrives
    ↓
Parse model name from request body
    ↓
Lookup InferenceModel by modelName
    ↓
┌─────────────────────────────────────┐
│ Deployment exists?                  │
│   Yes → Skip to proxy               │
│   No  → Create deployment           │
│         Wait for pod ready          │
│         Update state                │
└─────────────────────────────────────┘
    ↓
Proxy request to backend pod
    ↓
Return response to client
    ↓
Update lastRequest timestamp
```

### 3. State Tracking

In-memory state for tracking active models:

```go
type ModelState struct {
    ActiveRequests  int       // Current in-flight requests
    LastRequestTime time.Time // For idle detection
    DeploymentName  string    // Name of the deployment
    Ready           bool      // Is the backend ready?
}

type ProxyState struct {
    sync.RWMutex
    Models map[string]*ModelState
}
```

### 4. Idle Cleanup

Background goroutine that periodically checks for idle deployments:

```
Every 30 seconds:
  For each model in state:
    If activeRequests == 0 AND
       time.Since(lastRequestTime) > cooldownPeriod:
      Delete deployment
      Remove from state
```

## Open Questions

### 1. Proxy Port

What port should the proxy listen on?

- Option A: 8080 (common, may conflict with other services)
- Option B: 9000 (less common)
- Option C: Configurable via flag/env

### 2. Model Not Found

How to handle requests for unknown models?

- Option A: Return 404 error
- Option B: Auto-create InferenceModel from a template
- Option C: Return 503 with retry-after header

### 3. Deployment Lifecycle

Should we completely remove deployments when idle, or keep them at 0 replicas?

- **Remove completely**: Cleaner, truly serverless
- **Keep at 0 replicas**: Slightly faster warm start, more state to manage

**Recommendation**: Remove completely. The InferenceModel CRD is the source of truth.

### 4. Authentication

Should the proxy validate API keys?

- Option A: No auth (rely on network-level security)
- Option B: API key validation (check against secret)
- Option C: Delegate to backends (passthrough)

### 5. Multi-Replica Controller

If controller runs with multiple replicas:

- Option A: Leader election - only leader handles proxy traffic
- Option B: All replicas can proxy, share state via annotations
- Option C: Single replica (current default)

### 6. Request Timeout During Cold Start

How long to wait for a cold model to start?

Large models (70B+) can take several minutes to load. Options:

- Fixed timeout (e.g., 5 minutes)
- Per-model timeout from InferenceModel spec
- Streaming status updates to client

## Implementation Plan

### Phase 1: Basic Proxy
- HTTP server with `/v1/chat/completions` endpoint
- Parse model name, lookup InferenceModel
- Basic proxying to backend pods

### Phase 2: On-Demand Creation
- Create deployment if not exists
- Wait for pod readiness before proxying
- Handle cold start timeouts

### Phase 3: Idle Cleanup
- Background cleanup goroutine
- Track active requests
- Delete idle deployments

### Phase 4: Full API Coverage
- Add `/v1/completions`
- Add `/v1/embeddings`
- Add `/v1/audio/transcriptions`
- Add `/v1/models`

### Phase 5: Hardening
- Single-flight for concurrent cold requests
- Proper streaming (SSE) support
- Metrics and observability
- Error handling and retries

## Technical Details

### Streaming Responses

For streaming chat completions, use `httputil.ReverseProxy` which handles SSE automatically:

```go
proxy := httputil.NewSingleHostReverseRouter(backendURL)
proxy.FlushInterval = -1 // Flush immediately for streaming
proxy.ServeHTTP(w, r)
```

### Concurrent Cold Requests

Use single-flight pattern to prevent duplicate deployment creation:

```go
var sf singleflight.Group

func (p *Proxy) handleRequest(model string) {
    _, err, _ := sf.Do(model, func() (interface{}, error) {
        return p.ensureDeployment(model)
    })
}
```

### Pod Readiness Check

Wait for deployment to have at least one ready pod:

```go
func (p *Proxy) waitForReady(ctx context.Context, deploymentName string) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            deps := &appsv1.Deployment{}
            p.client.Get(ctx, client.Key{Name: deploymentName}, deps)
            if deps.Status.ReadyReplicas > 0 {
                return nil
            }
            time.Sleep(1 * time.Second)
        }
    }
}
```

## Benefits

1. **True serverless**: Zero resource usage when idle
2. **Simplified architecture**: One component instead of two
3. **Cost efficiency**: No idle compute resources
4. **Cleaner state**: Deployments only exist when needed
5. **OpenAI compatible**: Drop-in replacement for OpenAI API

## Trade-offs

1. **Cold start latency**: First request waits for pod startup + model loading
2. **Controller complexity**: More responsibilities in one component
3. **State is ephemeral**: Lost on controller restart (acceptable)
4. **Single point of failure**: If controller is down, no inference (unless multi-replica)
