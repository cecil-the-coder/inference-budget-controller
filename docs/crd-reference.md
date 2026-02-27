# InferenceModel CRD Reference

This document provides a complete reference for the InferenceModel Custom Resource Definition.

## API Version

`inference.eh-ops.io/v1alpha1`

## Resource Types

- `InferenceModel` - Represents a single inference model
- `InferenceModelList` - List of InferenceModel resources

## InferenceModel Spec

The `spec` field defines the desired state of an inference model.

### Required Fields

| Field | Type | Description | Validation |
|-------|------|-------------|------------|
| `modelName` | string | The name of the model used for API routing. This is the identifier clients use in their requests. | MinLength: 1 |
| `memory` | string | Memory requirement for this model (e.g., "80Gi"). Used for budget calculation and scheduling decisions. | Pattern: `^[0-9]+(Ki|Mi|Gi|Ti)$` |
| `backendUrl` | string | URL of the backend inference server that will handle requests. | Format: URI |

### Optional Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `nodeSelector` | map[string]string | `{"inference-pool": "default"}` | Labels to select which node pool this model runs on. Models sharing a node pool compete for the same memory budget. |
| `cooldownPeriod` | [metav1.Duration](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#Duration) | `10m` | How long to wait after the last request before scaling down to zero replicas. |
| `maxReplicas` | int32 | `1` | Maximum number of replicas. Currently limited to 0-1. Future versions will support higher values. |
| `dependencies` | []string | `[]` | List of model names that must be running before this model starts. Used for model pipelines. |
| `containerImage` | string | `""` | Docker image for the model server container. Required when the controller manages the deployment. |
| `resources` | [ResourceRequirements](#resourcerequirements) | `{}` | Compute resource requirements for the container. |

### ResourceRequirements

Defines compute resource requests and limits for the model container.

```yaml
resources:
  requests:
    cpu: "4"        # CPU request (e.g., "4", "4000m")
    memory: "16Gi"  # Memory request
    gpu: "1"        # GPU request (maps to nvidia.com/gpu)
  limits:
    cpu: "8"        # CPU limit
    memory: "20Gi"  # Memory limit
    gpu: "1"        # GPU limit
```

#### ResourceList Fields

| Field | Type | Description |
|-------|------|-------------|
| `cpu` | string | CPU requirement in Kubernetes format (e.g., "4", "4000m") |
| `memory` | string | Memory requirement (e.g., "16Gi", "16384Mi") |
| `gpu` | string | GPU count (e.g., "1", "2"). Maps to `nvidia.com/gpu` resource. |

### Example Spec

```yaml
spec:
  modelName: llama-2-70b
  memory: 80Gi
  backendUrl: http://vllm-server:8000
  nodeSelector:
    inference-pool: gpu-a100
    gpu-type: nvidia-a100
  cooldownPeriod: 15m
  maxReplicas: 1
  containerImage: vllm/vllm-openai:latest
  resources:
    requests:
      cpu: "8"
      memory: "80Gi"
      gpu: "1"
    limits:
      cpu: "16"
      memory: "96Gi"
      gpu: "1"
```

## InferenceModel Status

The `status` field reflects the observed state of the model.

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `ready` | boolean | Indicates if the model is ready to serve requests. True when at least one replica is ready. |
| `replicas` | int32 | Current number of running replicas (0 when scaled to zero). |
| `observedPeakMemory` | string | Peak memory observed at runtime (e.g., "75Gi"). Populated from metrics collection. |
| `utilizationPercent` | int32 | Ratio of observed to declared memory (0-100). Values below 90 suggest potential overprovisioning. |
| `recommendation` | string | Human-readable suggestion for right-sizing memory allocation. |
| `lastObservation` | [metav1.Time](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#Time) | Timestamp of the last metrics update. |
| `conditions` | ][metav1.Condition](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#Condition) | List of conditions describing the model's state. |

### Condition Types

| Type | Description |
|------|-------------|
| `Ready` | The model is ready to serve requests. |
| `Available` | The model deployment is available and healthy. |

### Condition Reasons

| Reason | Description |
|--------|-------------|
| `Deploying` | Deployment is in progress, waiting for pods to become ready. |
| `Deployed` | Deployment is complete and model is serving traffic. |
| `Failed` | Deployment failed due to an error. |
| `InsufficientMemory` | Cannot schedule due to insufficient memory budget. |
| `ScaledToZero` | Model is scaled to zero due to inactivity. |
| `Pending` | Deployment is pending. |
| `Terminating` | Model is being terminated. |

### Example Status

```yaml
status:
  ready: true
  replicas: 1
  observedPeakMemory: 75Gi
  utilizationPercent: 93
  recommendation: "Consider reducing memory from 80Gi to 78Gi (observed peak 75Gi, 6% overprovisioned)"
  lastObservation: "2024-01-15T10:30:00Z"
  conditions:
    - type: Ready
      status: "True"
      reason: Deployed
      message: "Deployment is ready and serving traffic"
      lastTransitionTime: "2024-01-15T10:00:00Z"
    - type: Available
      status: "True"
      reason: Deployed
      message: "Model is available and serving requests"
      lastTransitionTime: "2024-01-15T10:00:00Z"
```

## Validation Rules

### Memory Format

Memory must follow the Kubernetes quantity format with binary suffixes:

- `Ki` - Kibibytes (1024 bytes)
- `Mi` - Mebibytes (1024^2 bytes)
- `Gi` - Gibibytes (1024^3 bytes)
- `Ti` - Tebibytes (1024^4 bytes)

**Valid examples:**
- `16Gi`
- `16384Mi`
- `80Gi`
- `1Ti`

**Invalid examples:**
- `16GB` (use `Gi` instead)
- `80` (missing unit)
- `1.5Gi` (decimals not supported in pattern)

### Backend URL Format

The `backendUrl` must be a valid URI:

**Valid examples:**
- `http://vllm-server:8000`
- `http://vllm-server.default.svc.cluster.local:8000`
- `https://inference.example.com`

### MaxReplicas Constraints

Currently limited to 0-1:
- Minimum: 0
- Maximum: 1

Future versions will support higher values for horizontal scaling.

## Labels and Annotations

### Auto-generated Labels

The controller automatically adds these labels to managed Deployments:

| Label | Value |
|-------|-------|
| `app.kubernetes.io/name` | InferenceModel name |
| `app.kubernetes.io/component` | `inference-server` |
| `inference.eh-ops.io/model` | Value of `spec.modelName` |
| `inference.eh-ops.io/managed` | `true` |

### Auto-generated Annotations

| Annotation | Description |
|------------|-------------|
| `inference.eh-ops.io/memory` | Memory allocation from spec |
| `inference.eh-ops.io/last-request-time` | RFC3339 timestamp of last request |

### Using Labels for Selection

```bash
# Find all managed inference models
kubectl get deployments -l inference.eh-ops.io/managed=true

# Find a specific model
kubectl get deployments -l inference.eh-ops.io/model=llama-2-7b
```

## Printer Columns

The CRD includes additional printer columns for `kubectl get`:

```bash
kubectl get inferencemodels

NAME           READY   REPLICAS   MEMORY   AGE
llama-2-7b     true    1          16Gi     1h
llama-2-70b    false   0          80Gi     2d
mistral-7b     true    1          8Gi      30m
```

## Subresources

The InferenceModel CRD supports:

- `/status` - For updating status (controller use only)

```bash
# Update status (typically done by controller)
kubectl patch inferencemodel llama-2-7b --subresource=status -p '{"status":{"ready":true}}'
```

## Finalizers

The controller uses a finalizer for cleanup:

```
inference.eh-ops.io/finalizer
```

When an InferenceModel is deleted, the controller:
1. Detects the deletion timestamp
2. Releases the memory budget allocation
3. Cleans up metrics
4. Removes the finalizer
5. Allows the object to be deleted

## Full Example

```yaml
apiVersion: inference.eh-ops.io/v1alpha1
kind: InferenceModel
metadata:
  name: codellama-34b
  labels:
    environment: production
    team: ml-platform
spec:
  # Required fields
  modelName: codellama-34b-instruct
  memory: 48Gi
  backendUrl: http://vllm-server.ml-platform.svc.cluster.local:8000

  # Node placement
  nodeSelector:
    inference-pool: gpu-a100
    accelerator: nvidia-a100-80gb

  # Scaling configuration
  cooldownPeriod: 20m
  maxReplicas: 1

  # Model dependencies (must be running before this model)
  dependencies:
    - tokenizer-service

  # Container configuration
  containerImage: vllm/vllm-openai:v0.3.0
  resources:
    requests:
      cpu: "8"
      memory: "48Gi"
      gpu: "1"
    limits:
      cpu: "16"
      memory: "64Gi"
      gpu: "1"
