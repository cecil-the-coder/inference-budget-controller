# Getting Started

This guide walks you through installing and using the Inference Budget Controller.

## Prerequisites

- **Kubernetes**: Version 1.25 or later
- **kubectl**: Configured to communicate with your cluster
- **Go**: Version 1.26+ (for building from source)
- **Docker**: For building container images

### Optional

- **Helm**: Version 3.0+ (for Helm-based installation)
- **Prometheus**: For metrics collection and observability

## Installation

### Option 1: Quick Install with kubectl

Apply the pre-built manifests directly:

```bash
# Install the CRD
kubectl apply -f https://raw.githubusercontent.com/cecil-the-coder/inference-budget-controller/main/config/crd/bases/inference.eh-ops.io_inferencemodels.yaml

# Install the controller deployment
kubectl apply -f https://raw.githubusercontent.com/cecil-the-coder/inference-budget-controller/main/config/manager/manager.yaml
kubectl apply -f https://raw.githubusercontent.com/cecil-the-coder/inference-budget-controller/main/config/rbac/
```

### Option 2: Build and Deploy from Source

```bash
# Clone the repository
git clone https://github.com/cecil-the-coder/inference-budget-controller.git
cd inference-budget-controller

# Download dependencies
go mod download

# Build the controller
go build -o bin/manager ./cmd/main.go

# Build the Docker image
docker build -t ghcr.io/cecil-the-coder/inference-budget-controller:latest .

# Push the image to your registry
docker push ghcr.io/cecil-the-coder/inference-budget-controller:latest

# Deploy using kustomize
kubectl apply -k config/default
```

### Option 3: Use Pre-built Image

```bash
# Pull the pre-built image
docker pull ghcr.io/cecil-the-coder/inference-budget-controller:latest

# Deploy using the sample manifests
kubectl apply -f https://raw.githubusercontent.com/cecil-the-coder/inference-budget-controller/main/config/crd/bases/inference.eh-ops.io_inferencemodels.yaml
kubectl apply -f https://raw.githubusercontent.com/cecil-the-coder/inference-budget-controller/main/config/rbac/
kubectl apply -f https://raw.githubusercontent.com/cecil-the-coder/inference-budget-controller/main/config/manager/manager.yaml
```

## Quick Start Example

### 1. Verify the Controller is Running

```bash
# Check the controller pod status
kubectl get pods -n inference-budget-controller-system

# Expected output:
NAME                                           READY   STATUS    RESTARTS   AGE
inference-budget-controller-controller-manager   1/1     Running   0          30s
```

### 2. Create Your First InferenceModel

Save the following YAML as `my-model.yaml`:

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
  containerImage: vllm/vllm-openai:latest
  resources:
    requests:
      cpu: "4"
      memory: "16Gi"
      gpu: "1"
    limits:
      cpu: "8"
      memory: "20Gi"
      gpu: "1"
```

Apply it to your cluster:

```bash
kubectl apply -f my-model.yaml
```

### 3. Verify the InferenceModel

```bash
# List all InferenceModels
kubectl get inferencemodels

# Check the status of your model
kubectl get inferencemodel llama-2-7b -o yaml

# Expected output includes:
# status:
#   ready: true
#   replicas: 1
```

### 4. Send a Test Request

If you have the API Proxy enabled, you can send requests to the OpenAI-compatible endpoint:

```bash
# Port-forward the proxy service (adjust port as needed)
kubectl port-forward svc/inference-budget-controller-proxy 8080:8080 -n inference-budget-controller-system

# Send a chat completion request
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-2-7b",
    "messages": [{"role": "user", "content": "Hello, world!"}]
  }'
```

### 5. Observe Scaling Behavior

Watch the model scale to zero after being idle:

```bash
# Watch the deployment replicas
kubectl get deployment llama-2-7b -w

# After 10 minutes of inactivity (default cooldownPeriod):
# The deployment will scale to 0 replicas
```

When a new request arrives, the controller will:
1. Check if memory budget is available
2. Scale up the deployment
3. Wait for readiness
4. Forward the request

## Verify Installation

### Check CRD Registration

```bash
kubectl get crd inferencemodels.inference.eh-ops.io
```

### Check Controller Logs

```bash
# View controller logs
kubectl logs -n inference-budget-controller-system \
  -l control-plane=controller-manager \
  -f
```

### Check Health Endpoints

```bash
# Port-forward to access health endpoints
kubectl port-forward -n inference-budget-controller-system \
  deployment/inference-budget-controller-controller-manager 8081:8081

# Check health
curl http://localhost:8081/healthz
# Expected: {"status": "ok"}

curl http://localhost:8081/readyz
# Expected: {"status": "ready"}
```

## Next Steps

- [CRD Reference](./crd-reference.md) - Detailed specification of all InferenceModel fields
- [Architecture](./architecture.md) - Understanding how components interact
- [Admission Control](./admission-control.md) - How 429 responses work
- [Observability](./observability.md) - Setting up monitoring and alerts
- [Examples](./examples/) - More configuration examples

## Troubleshooting

### Controller Pod Not Starting

```bash
# Check pod events
kubectl describe pod -n inference-budget-controller-system \
  -l control-plane=controller-manager

# Check for RBAC issues
kubectl get clusterrolebinding inference-budget-controller-manager-rolebinding
```

### InferenceModel Stuck in Pending

```bash
# Check model conditions
kubectl describe inferencemodel <model-name>

# Check events
kubectl get events --field-selector involvedObject.name=<model-name>
```

### 429 Insufficient Memory Error

This means the memory budget is exhausted. Check:

```bash
# View all models and their memory allocations
kubectl get inferencemodels -o custom-columns=NAME:.metadata.name,MEMORY:.spec.memory,READY:.status.ready

# Check node pool capacity
kubectl describe node <node-name> | grep -A 5 "Allocated resources"
```

Consider:
- Increasing node pool capacity
- Reducing memory allocation for other models
- Waiting for idle models to scale down
