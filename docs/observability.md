# Observability

This document covers monitoring, metrics, logging, and alerting for the Inference Budget Controller.

## Prometheus Metrics

The controller exposes Prometheus metrics on port 8080 by default.

### Model Metrics

#### `inference_model_memory_declared_bytes`

Memory declared in the InferenceModel CRD.

| Label | Description |
|-------|-------------|
| `model` | InferenceModel name |
| `namespace` | Kubernetes namespace |

```promql
# Example query: Get declared memory for all models
inference_model_memory_declared_bytes

# Example query: Total declared memory
sum(inference_model_memory_declared_bytes)
```

#### `inference_model_memory_observed_peak_bytes`

Peak memory observed at runtime for each model.

| Label | Description |
|-------|-------------|
| `model` | InferenceModel name |
| `namespace` | Kubernetes namespace |

```promql
# Example query: Get observed peak memory
inference_model_memory_observed_peak_bytes

# Example query: Models with peak > 50Gi
inference_model_memory_observed_peak_bytes > 50 * 1024 * 1024 * 1024
```

#### `inference_model_memory_utilization_ratio`

Ratio of observed peak memory to declared memory (0-1). Values below 0.9 suggest overprovisioning.

| Label | Description |
|-------|-------------|
| `model` | InferenceModel name |
| `namespace` | Kubernetes namespace |

```promql
# Example query: Find underutilized models (< 70% utilization)
inference_model_memory_utilization_ratio < 0.7

# Example query: Average utilization across all models
avg(inference_model_memory_utilization_ratio)
```

#### `inference_model_ready`

Whether the model is ready to serve requests (1=ready, 0=not ready).

| Label | Description |
|-------|-------------|
| `model` | InferenceModel name |
| `namespace` | Kubernetes namespace |

```promql
# Example query: Count ready models
sum(inference_model_ready)

# Example query: Find models that are not ready
inference_model_ready == 0

# Example query: Percentage of models ready
avg(inference_model_ready) * 100
```

#### `inference_model_replicas`

Current number of replicas for each model.

| Label | Description |
|-------|-------------|
| `model` | InferenceModel name |
| `namespace` | Kubernetes namespace |

```promql
# Example query: Total running replicas
sum(inference_model_replicas)

# Example query: Models scaled to zero
inference_model_replicas == 0
```

### Budget Metrics

#### `inference_budget_total_bytes`

Total memory budget for each node pool.

| Label | Description |
|-------|-------------|
| `node_pool` | Node pool identifier (from nodeSelector) |

```promql
# Example query: Total budget by pool
inference_budget_total_bytes

# Example query: Total cluster budget
sum(inference_budget_total_bytes)
```

#### `inference_budget_allocated_bytes`

Currently allocated memory in each node pool.

| Label | Description |
|-------|-------------|
| `node_pool` | Node pool identifier |

```promql
# Example query: Allocated memory by pool
inference_budget_allocated_bytes

# Example query: Allocation percentage by pool
inference_budget_allocated_bytes / inference_budget_total_bytes * 100
```

#### `inference_budget_available_bytes`

Available (unallocated) memory in each node pool.

| Label | Description |
|-------|-------------|
| `node_pool` | Node pool identifier |

```promql
# Example query: Available memory by pool
inference_budget_available_bytes

# Example query: Pools with less than 20Gi available
inference_budget_available_bytes < 20 * 1024 * 1024 * 1024
```

## Grafana Dashboard

### Dashboard JSON

Import this dashboard to visualize inference budget metrics:

```json
{
  "annotations": {
    "list": []
  },
  "title": "Inference Budget Controller",
  "uid": "inference-budget",
  "version": 1,
  "panels": [
    {
      "title": "Memory Budget Overview",
      "type": "gauge",
      "gridPos": {"h": 8, "w": 8, "x": 0, "y": 0},
      "targets": [
        {
          "expr": "sum(inference_budget_allocated_bytes) / sum(inference_budget_total_bytes) * 100",
          "legendFormat": "Allocated %"
        }
      ],
      "fieldConfig": {
        "defaults": {
          "max": 100,
          "min": 0,
          "unit": "percent",
          "thresholds": {
            "mode": "absolute",
            "steps": [
              {"color": "green", "value": 0},
              {"color": "yellow", "value": 70},
              {"color": "red", "value": 90}
            ]
          }
        }
      }
    },
    {
      "title": "Model Status",
      "type": "table",
      "gridPos": {"h": 8, "w": 16, "x": 8, "y": 0},
      "targets": [
        {
          "expr": "inference_model_ready",
          "format": "table",
          "instant": true
        },
        {
          "expr": "inference_model_replicas",
          "format": "table",
          "instant": true
        },
        {
          "expr": "inference_model_memory_declared_bytes",
          "format": "table",
          "instant": true
        }
      ],
      "transformations": [
        {
          "id": "seriesToColumns",
          "options": {"byField": "model"}
        }
      ]
    },
    {
      "title": "Memory Utilization",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8},
      "targets": [
        {
          "expr": "inference_model_memory_utilization_ratio * 100",
          "legendFormat": "{{model}}"
        }
      ],
      "fieldConfig": {
        "defaults": {
          "unit": "percent",
          "max": 100,
          "min": 0
        }
      }
    },
    {
      "title": "Budget Allocation Over Time",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8},
      "targets": [
        {
          "expr": "inference_budget_allocated_bytes",
          "legendFormat": "{{node_pool}} - Allocated"
        },
        {
          "expr": "inference_budget_total_bytes",
          "legendFormat": "{{node_pool}} - Total"
        }
      ],
      "fieldConfig": {
        "defaults": {
          "unit": "bytes"
        }
      }
    },
    {
      "title": "Models by Status",
      "type": "piechart",
      "gridPos": {"h": 8, "w": 8, "x": 0, "y": 16},
      "targets": [
        {
          "expr": "sum(inference_model_ready)",
          "legendFormat": "Ready"
        },
        {
          "expr": "count(inference_model_ready == 0) or vector(0)",
          "legendFormat": "Not Ready"
        }
      ]
    },
    {
      "title": "Overprovisioned Models",
      "type": "stat",
      "gridPos": {"h": 8, "w": 8, "x": 8, "y": 16},
      "targets": [
        {
          "expr": "count(inference_model_memory_utilization_ratio < 0.9) or vector(0)",
          "legendFormat": "Models with < 90% utilization"
        }
      ],
      "fieldConfig": {
        "defaults": {
          "thresholds": {
            "mode": "absolute",
            "steps": [
              {"color": "green", "value": 0},
              {"color": "yellow", "value": 1},
              {"color": "red", "value": 5}
            ]
          }
        }
      }
    },
    {
      "title": "Available Memory",
      "type": "stat",
      "gridPos": {"h": 8, "w": 8, "x": 16, "y": 16},
      "targets": [
        {
          "expr": "sum(inference_budget_available_bytes)",
          "legendFormat": "Total Available"
        }
      ],
      "fieldConfig": {
        "defaults": {
          "unit": "bytes"
        }
      }
    }
  ],
  "schemaVersion": 38,
  "refresh": "30s"
}
```

## Logging

### Log Format

The controller uses structured JSON logging via controller-runtime:

```json
{
  "level": "info",
  "ts": "2024-01-15T10:30:00.123Z",
  "logger": "controller.inferencemodel",
  "msg": "Created deployment for InferenceModel",
  "model": "llama-2-7b",
  "namespace": "default"
}
```

### Log Levels

| Level | Description |
|-------|-------------|
| `error` | Errors that prevent normal operation |
| `warn` | Warning conditions that don't stop operation |
| `info` | General operational information |
| `debug` | Detailed debugging information |

### Key Log Messages

#### Model Lifecycle

```
# Model creation
INFO  Created deployment for InferenceModel  model=llama-2-7b namespace=default

# Model scaling
INFO  Scaled deployment to zero replicas  model=llama-2-7b reason="idle timeout"
INFO  Triggered scale-up for deployment  deployment=llama-2-7b

# Model deletion
INFO  Handling InferenceModel deletion, releasing memory budget  model=llama-2-7b
INFO  Finalizer removed, InferenceModel can be deleted  model=llama-2-7b
```

#### Budget Events

```
# Insufficient memory
INFO  Insufficient memory budget, cannot create deployment  model=codellama-34b memory=48Gi

# Memory allocation
INFO  Memory budget allocated  model=llama-2-7b memory=16Gi nodePool=gpu-a100
INFO  Memory budget released  model=llama-2-7b reason="scaled to zero"
```

#### Proxy Events

```
# Request handling
INFO  Model is now ready  model=llama-2-7b
INFO  Insufficient memory for model  model=codellama-34b available=32Gi blocking=2
```

### Viewing Logs

```bash
# View all controller logs
kubectl logs -n inference-budget-controller-system \
  -l control-plane=controller-manager -f

# Filter for specific model
kubectl logs -n inference-budget-controller-system \
  -l control-plane=controller-manager | jq 'select(.model == "llama-2-7b")'

# View only errors
kubectl logs -n inference-budget-controller-system \
  -l control-plane=controller-manager | jq 'select(.level == "error")'
```

## Alerting Rules

### Prometheus Alert Rules

```yaml
groups:
  - name: inference-budget-controller
    rules:
      # Critical: No models ready
      - alert: NoInferenceModelsReady
        expr: sum(inference_model_ready) == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: No inference models are ready
          description: "All inference models are in a not-ready state. Check controller logs."

      # Warning: High memory utilization
      - alert: HighMemoryUtilization
        expr: |
          sum(inference_budget_allocated_bytes) / sum(inference_budget_total_bytes) > 0.9
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: Memory budget is nearly exhausted
          description: "{{ $value | humanizePercentage }} of memory budget is allocated. Consider adding capacity."

      # Warning: Model stuck not ready
      - alert: ModelNotReady
        expr: inference_model_ready == 0 and inference_model_replicas > 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: Model {{ $labels.model }} is not becoming ready
          description: "Model {{ $labels.model }} in namespace {{ $labels.namespace }} has replicas but is not ready."

      # Info: Overprovisioned models
      - alert: ModelOverprovisioned
        expr: inference_model_memory_utilization_ratio < 0.7
        for: 1h
        labels:
          severity: info
        annotations:
          summary: Model {{ $labels.model }} is overprovisioned
          description: "Model {{ $labels.model }} is using only {{ $value | humanizePercentage }} of declared memory. Consider reducing allocation."

      # Warning: Frequent scale-ups
      - alert: FrequentScaleUps
        expr: |
          increase(inference_model_replicas[5m]) > 3
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: Model {{ $labels.model }} is scaling up frequently
          description: "Consider adjusting cooldown period or pre-warming this model."

      # Critical: Budget tracker inconsistency
      - alert: BudgetAllocationExceedsTotal
        expr: inference_budget_allocated_bytes > inference_budget_total_bytes
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: Budget allocation exceeds total capacity
          description: "Node pool {{ $labels.node_pool }} has allocated more memory than available. Controller may need restart."
```

## Recommendation Interpretation

The controller provides recommendations for right-sizing memory allocations in the CRD status.

### Status Fields

```yaml
status:
  observedPeakMemory: 75Gi
  utilizationPercent: 93
  recommendation: "Consider reducing memory from 80Gi to 78Gi (observed peak 75Gi, 6% overprovisioned)"
```

### Understanding Recommendations

| Utilization | Interpretation | Action |
|-------------|----------------|--------|
| 90-100% | Well-provisioned | No action needed |
| 70-90% | Slightly overprovisioned | Consider slight reduction |
| 50-70% | Moderately overprovisioned | Review and reduce allocation |
| < 50% | Significantly overprovisioned | Reduce allocation significantly |

### Applying Recommendations

1. **Review the recommendation**:
   ```bash
   kubectl get inferencemodel llama-2-7b -o jsonpath='{.status.recommendation}'
   ```

2. **Update the memory allocation**:
   ```yaml
   # Before
   spec:
     memory: 80Gi

   # After (applying recommendation)
   spec:
     memory: 78Gi
   ```

3. **Monitor after changes**:
   ```bash
   # Watch for any issues
   kubectl logs -n inference-budget-controller-system \
     -l control-plane=controller-manager -f | grep llama-2-7b
   ```

### Safety Considerations

- **Add a buffer**: The controller's recommendation includes a 10% safety margin
- **Monitor after changes**: Watch for OOM events after reducing allocation
- **Gradual adjustments**: Make small changes and verify stability
- **Peak workload awareness**: Recommendations are based on observed peaks; consider your peak usage patterns

## Debugging

### Common Issues

#### Model Stuck in Pending

```bash
# Check conditions
kubectl describe inferencemodel <model-name>

# Look for InsufficientMemory reason
kubectl get inferencemodel <model-name> -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'
```

#### Memory Budget Not Released

```bash
# Check if model is deleted but finalizer stuck
kubectl get inferencemodel <model-name> -o yaml | grep -A5 deletionTimestamp

# Check controller logs for finalizer issues
kubectl logs -n inference-budget-controller-system \
  -l control-plane=controller-manager | grep finalizer
```

#### Metrics Not Appearing

```bash
# Verify metrics endpoint is accessible
kubectl port-forward -n inference-budget-controller-system \
  svc/inference-budget-controller-controller-manager-metrics-service 8080:8080

# Query metrics
curl http://localhost:8080/metrics | grep inference_
```

### Useful Queries

```promql
# Memory efficiency across all models
avg(inference_model_memory_utilization_ratio)

# Total cluster memory usage
sum(inference_budget_allocated_bytes) / 1024 / 1024 / 1024  # in GiB

# Models that could fit in freed memory
sum(inference_budget_available_bytes) / avg(inference_model_memory_declared_bytes)

# Time until next scale-down (approximate)
# Requires custom metric for last request time
```
