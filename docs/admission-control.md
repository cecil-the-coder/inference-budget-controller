# Admission Control

This document explains how the Inference Budget Controller handles admission control for inference requests.

## Overview

Admission control is the process of deciding whether to accept or reject a request based on resource availability. The Inference Budget Controller implements admission control through the API Proxy, which intercepts all requests and checks memory budgets before allowing them to proceed.

## How Admission Control Works

### Request Lifecycle

```
+------------------+
| Client Request   |
| (model: llama-2) |
+------------------+
         |
         v
+------------------+
| API Proxy        |
| receives request |
+------------------+
         |
         v
+------------------+     +------------------+
| Lookup Model     |---->| Model Not Found  |---> 404 Not Found
| in Kubernetes    |     +------------------+
+------------------+
         | Found
         v
+------------------+
| Check Model      |
| Ready Status     |
+------------------+
         |
         +------------------------------------+
         |                                    |
         v                                    v
+------------------+                 +------------------+
| Model is Ready   |                 | Model Not Ready  |
| Forward Request  |                 | Check Budget     |
+------------------+                 +------------------+
         |                                    |
         v                                    v
+------------------+                 +------------------+
| Backend Server   |                 | Budget Available?|
| Processes Request|                 +------------------+
+------------------+                         |
         |                          +--------+--------+
         |                          |                 |
         v                          v                 v
+------------------+        +----------------+  +------------------+
| Return Response  |        | Scale Up Model | | Return 429       |
| to Client        |        | Wait for Ready | | Insufficient     |
+------------------+        +----------------+ | Memory           |
                                    |         +------------------+
                                    v
                            +------------------+
                            | Forward Request  |
                            +------------------+
```

### Memory Budget Check

When a request arrives for a model that is scaled to zero:

1. **Calculate Required Memory**: Get the `spec.memory` from the InferenceModel CRD
2. **Check Node Budget**: Query the Budget Tracker for available memory on the target node pool
3. **Decision**:
   - If `available >= required`: Proceed with scale-up
   - If `available < required`: Return 429 immediately

### Budget Calculation

```
Total Node Pool Memory:     128Gi
Currently Allocated:        -80Gi (Model A)
----------------------------
Available Memory:            48Gi

New Model Request:           60Gi
Available (48Gi) < Required (60Gi) --> 429 Response
```

## 429 Response Format

When a request cannot be fulfilled due to insufficient memory, the API returns a 429 Too Many Requests response with detailed information.

### Response Structure

```json
{
  "error": {
    "type": "insufficient_memory",
    "message": "Cannot schedule llama-2-70b: insufficient memory",
    "code": "insufficient_memory",
    "details": {
      "requested": {
        "model": "llama-2-70b",
        "memory": "80Gi"
      },
      "available": "48Gi",
      "blocking": [
        {
          "model": "codellama-34b",
          "memory": "48Gi",
          "idle_for": "5m30s"
        },
        {
          "model": "mistral-7b",
          "memory": "32Gi",
          "idle_for": "2m15s"
        }
      ]
    }
  }
}
```

### Field Descriptions

| Field | Type | Description |
|-------|------|-------------|
| `error.type` | string | Error type identifier, always `insufficient_memory` |
| `error.message` | string | Human-readable error message |
| `error.code` | string | Machine-readable error code |
| `error.details.requested.model` | string | Name of the requested model |
| `error.details.requested.memory` | string | Memory required by the requested model |
| `error.details.available` | string | Currently available memory on the node pool |
| `error.details.blocking` | array | List of models currently using memory |
| `error.details.blocking[].model` | string | Name of the blocking model |
| `error.details.blocking[].memory` | string | Memory allocated to the blocking model |
| `error.details.blocking[].idle_for` | string | How long since the blocking model's last request |

### HTTP Headers

The 429 response includes these headers:

```
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 300
```

The `Retry-After` header suggests when the client might retry (based on the shortest idle time among blocking models).

## Client Retry Strategies

### Exponential Backoff

Recommended approach for handling 429 responses:

```python
import time
import random

def request_with_backoff(client, model, messages, max_retries=5):
    base_delay = 1  # Start with 1 second
    max_delay = 300  # Max 5 minutes

    for attempt in range(max_retries):
        response = client.chat.completions.create(
            model=model,
            messages=messages
        )

        if response.status_code != 429:
            return response

        # Parse retry-after header or use exponential backoff
        retry_after = response.headers.get('Retry-After')
        if retry_after:
            delay = int(retry_after)
        else:
            delay = min(base_delay * (2 ** attempt), max_delay)
            delay = delay + random.uniform(0, 1)  # Add jitter

        time.sleep(delay)

    raise Exception(f"Max retries ({max_retries}) exceeded")
```

### Check Blocking Models

Use the blocking model information to make smarter decisions:

```python
import requests

def smart_retry(model, messages):
    max_wait_for_scale_down = 600  # 10 minutes

    response = requests.post(
        "http://proxy:8080/v1/chat/completions",
        json={"model": model, "messages": messages}
    )

    if response.status_code == 429:
        error = response.json()["error"]
        blocking = error["details"]["blocking"]

        # Find the blocking model that will scale down soonest
        min_idle = min(
            parse_duration(b["idle_for"])
            for b in blocking
        )
        cooldown = parse_duration("10m")  # Default cooldown

        # Wait until one model scales down
        wait_time = max(0, cooldown - min_idle)

        if wait_time < max_wait_for_scale_down:
            time.sleep(wait_time)
            return smart_retry(model, messages)
        else:
            raise Exception("Would wait too long, try a different model")

    return response.json()
```

### Circuit Breaker Pattern

Implement a circuit breaker to avoid overwhelming the proxy:

```python
from datetime import datetime, timedelta
from collections import defaultdict

class InferenceCircuitBreaker:
    def __init__(self, failure_threshold=3, timeout=300):
        self.failure_threshold = failure_threshold
        self.timeout = timeout
        self.failures = defaultdict(int)
        self.last_failure = defaultdict(datetime)
        self.circuit_open = defaultdict(bool)

    def call(self, model, request_func):
        if self.circuit_open[model]:
            if datetime.now() - self.last_failure[model] > timedelta(seconds=self.timeout):
                # Try to close circuit
                self.circuit_open[model] = False
                self.failures[model] = 0
            else:
                raise Exception(f"Circuit open for model {model}")

        try:
            result = request_func()
            self.failures[model] = 0
            return result
        except Exception as e:
            self.failures[model] += 1
            self.last_failure[model] = datetime.now()

            if self.failures[model] >= self.failure_threshold:
                self.circuit_open[model] = True

            raise
```

### Async Retry with aiohttp

For high-throughput async applications:

```python
import asyncio
import aiohttp
from exponential_backoff import Backoff

async def async_request_with_retry(session, url, payload, max_retries=5):
    backoff = Backoff(
        first=1.0,
        maximum=300.0,
        jitter=True
    )

    for attempt in range(max_retries):
        async with session.post(url, json=payload) as response:
            if response.status == 429:
                data = await response.json()
                blocking = data.get("error", {}).get("details", {}).get("blocking", [])

                # Calculate wait time based on blocking models
                if blocking:
                    min_idle = min(parse_duration(b["idle_for"]) for b in blocking)
                    cooldown = 600  # 10 minutes default
                    wait_time = max(0, cooldown - min_idle)
                else:
                    wait_time = backoff.next()

                await asyncio.sleep(wait_time)
                continue

            if response.status == 200:
                return await response.json()

            raise Exception(f"Unexpected status: {response.status}")

    raise Exception(f"Max retries exceeded")
```

## Error Response Types

Besides 429, the proxy may return other error responses:

### Model Not Found (404)

```json
{
  "error": {
    "message": "Model 'unknown-model' not found",
    "type": "invalid_request_error",
    "code": "model_not_found"
  }
}
```

### Invalid Request (400)

```json
{
  "error": {
    "message": "Invalid request: missing required field 'messages'",
    "type": "invalid_request_error",
    "code": "invalid_request"
  }
}
```

### Model Not Ready (503)

```json
{
  "error": {
    "message": "Model failed to become ready: timeout waiting for model",
    "type": "server_error",
    "code": "model_not_ready"
  }
}
```

### Backend Unavailable (502)

```json
{
  "error": {
    "message": "Failed to connect to backend: connection refused",
    "type": "server_error",
    "code": "backend_unavailable"
  }
}
```

### Scale-Up Failed (500)

```json
{
  "error": {
    "message": "Failed to scale up model: failed to update deployment",
    "type": "server_error",
    "code": "scale_up_failed"
  }
}
```

## Best Practices

### 1. Handle 429 Gracefully

Always implement retry logic when calling the inference API. A 429 response is not a permanent error.

### 2. Use Appropriate Timeouts

Scale-up can take several minutes. Configure client timeouts accordingly:

```python
# For synchronous clients
client = OpenAI(timeout=600.0)  # 10 minutes

# For async clients
async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=600)) as session:
    ...
```

### 3. Log Blocking Model Information

When you receive a 429, log the blocking models for debugging:

```python
if response.status_code == 429:
    error_data = response.json()
    logger.warning(
        "Insufficient memory for model %s",
        model,
        extra={
            "available": error_data["error"]["details"]["available"],
            "blocking": error_data["error"]["details"]["blocking"]
        }
    )
```

### 4. Consider Model Priority

If you control multiple models, consider:
- Using lower-memory models as fallbacks
- Implementing priority queues for critical requests
- Pre-warming frequently-used models

### 5. Monitor Metrics

Set up alerts on 429 rates:

```yaml
# Prometheus alert example
groups:
  - name: inference-alerts
    rules:
      - alert: HighInference429Rate
        expr: |
          rate(http_requests_total{status="429", handler="chat_completions"}[5m]) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: High rate of 429 responses for inference requests
          description: "{{ $value }} requests/sec are being rejected due to insufficient memory"
```

## Configuration Options

### Controller Configuration

The following parameters affect admission control behavior:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `scaleUpTimeout` | 5m | Max time to wait for model to become ready |
| `readyCheckDelay` | 2s | Interval between readiness checks during scale-up |
| `cooldownPeriod` | 10m | Default idle time before scale-to-zero |

### Node Pool Budgets

Set memory budgets for node pools using node labels:

```yaml
# Node with label
metadata:
  labels:
    inference-pool: gpu-a100

# InferenceModel targeting this pool
spec:
  nodeSelector:
    inference-pool: gpu-a100
  memory: 80Gi  # Must fit within pool budget
```

The total budget for a node pool is typically the sum of GPU memory on nodes with matching labels.
