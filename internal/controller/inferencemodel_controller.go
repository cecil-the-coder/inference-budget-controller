/*
Copyright 2024 eh-ops.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
	"github.com/cecil-the-coder/inference-budget-controller/internal/budget"
	"github.com/cecil-the-coder/inference-budget-controller/internal/metrics"
)

const (
	// FinalizerName is the finalizer used for cleanup
	FinalizerName = "inference.eh-ops.io/finalizer"

	// DefaultContainerPort is the default port for the inference container
	DefaultContainerPort = 8080

	// LastRequestAnnotation is the annotation key for tracking last request time
	LastRequestAnnotation = "inference.eh-ops.io/last-request-time"

	// DefaultRequeueInterval is the default interval for requeuing reconciliations
	DefaultRequeueInterval = 30 * time.Second

	// IdleCheckInterval is how often to check for idle models
	IdleCheckInterval = 1 * time.Minute
)

// Condition types for InferenceModel status
const (
	ConditionTypeReady     = "Ready"
	ConditionTypeAvailable = "Available"
)

// Condition reasons
const (
	ReasonDeploying       = "Deploying"
	ReasonDeployed        = "Deployed"
	ReasonFailed          = "Failed"
	ReasonInsufficientMem = "InsufficientMemory"
	ReasonScaledToZero    = "ScaledToZero"
	ReasonPending         = "Pending"
	ReasonTerminating     = "Terminating"
)

// InferenceModelReconciler reconciles a InferenceModel object
type InferenceModelReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Tracker  *budget.Tracker
	Metrics  *metrics.Collector
}

//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencemodels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencemodels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencemodels/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *InferenceModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the InferenceModel instance
	model := &inferencev1alpha1.InferenceModel{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if errors.IsNotFound(err) {
			// Object was deleted - cleanup is handled via finalizer
			logger.Info("InferenceModel not found, cleanup should have been done via finalizer")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch InferenceModel")
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer
	if !model.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, model)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(model, FinalizerName) {
		if err := r.addFinalizer(ctx, model); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		logger.Info("Added finalizer to InferenceModel")
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if deployment exists
	deployment := &appsv1.Deployment{}
	deploymentName := req.Name
	deploymentKey := client.ObjectKey{Name: deploymentName, Namespace: req.Namespace}

	err := r.Get(ctx, deploymentKey, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			// Deployment doesn't exist yet, create it
			return r.createDeployment(ctx, model)
		}
		logger.Error(err, "unable to fetch Deployment")
		return ctrl.Result{}, err
	}

	// Check for scale-to-zero based on idle time
	if result, err := r.handleIdleScaling(ctx, model, deployment); err != nil {
		return result, err
	} else if result.Requeue || result.RequeueAfter > 0 {
		return result, nil
	}

	// Check if deployment needs to be updated
	if result, err := r.handleUpdate(ctx, model, deployment); err != nil {
		return result, err
	} else if result.Requeue || result.RequeueAfter > 0 {
		return result, nil
	}

	// Update status based on deployment state
	return r.updateStatus(ctx, model, deployment)
}

// handleDeletion handles the deletion of an InferenceModel
func (r *InferenceModelReconciler) handleDeletion(ctx context.Context, model *inferencev1alpha1.InferenceModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(model, FinalizerName) {
		// Finalizer already removed, nothing to do
		return ctrl.Result{}, nil
	}

	logger.Info("Handling InferenceModel deletion, releasing memory budget",
		"model", model.Name,
		"namespace", model.Namespace,
		"memory_declared", model.Spec.Memory,
		"utilization_percent", model.Status.UtilizationPercent,
	)

	// Release memory budget
	r.Tracker.ReleaseModel(model.Name, model.Namespace)

	// Clean up metrics
	r.Metrics.DeleteModelMetrics(model.Name, model.Namespace)

	// Remove finalizer
	if err := r.removeFinalizer(ctx, model); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	logger.Info("Finalizer removed, InferenceModel can be deleted")
	r.Recorder.Event(model, corev1.EventTypeNormal, "Deleted", "Memory budget released and finalizer removed")

	return ctrl.Result{}, nil
}

// addFinalizer adds the finalizer to the InferenceModel
func (r *InferenceModelReconciler) addFinalizer(ctx context.Context, model *inferencev1alpha1.InferenceModel) error {
	modelCopy := model.DeepCopy()
	controllerutil.AddFinalizer(modelCopy, FinalizerName)
	if err := r.Update(ctx, modelCopy); err != nil {
		return err
	}
	// Update the local copy
	controllerutil.AddFinalizer(model, FinalizerName)
	return nil
}

// removeFinalizer removes the finalizer from the InferenceModel
func (r *InferenceModelReconciler) removeFinalizer(ctx context.Context, model *inferencev1alpha1.InferenceModel) error {
	modelCopy := model.DeepCopy()
	controllerutil.RemoveFinalizer(modelCopy, FinalizerName)
	return r.Update(ctx, modelCopy)
}

// createDeployment creates a new deployment for the InferenceModel
func (r *InferenceModelReconciler) createDeployment(ctx context.Context, model *inferencev1alpha1.InferenceModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if we have enough memory budget
	if !r.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector) {
		logger.Info("Insufficient memory budget, cannot create deployment",
			"model", model.Name,
			"namespace", model.Namespace,
			"memory_declared", model.Spec.Memory,
			"node", getNodeSelectorKey(model.Spec.NodeSelector))

		r.Recorder.Event(model, corev1.EventTypeWarning, "InsufficientMemory",
			"Cannot schedule model: insufficient memory budget")

		// Update status to indicate pending due to insufficient memory
		if err := r.setCondition(ctx, model, ConditionTypeReady, metav1.ConditionFalse,
			ReasonInsufficientMem, "Cannot schedule model: insufficient memory budget"); err != nil {
			logger.Error(err, "failed to update status condition")
		}

		// Requeue to retry later
		return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
	}

	// Build the deployment spec
	deployment := r.buildDeployment(model)

	// Set owner reference
	if err := controllerutil.SetControllerReference(model, deployment, r.Scheme); err != nil {
		logger.Error(err, "failed to set controller reference")
		return ctrl.Result{}, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create the deployment
	if err := r.Client.Create(ctx, deployment); err != nil {
		if errors.IsAlreadyExists(err) {
			// Deployment already exists, requeue to handle it in next iteration
			logger.Info("Deployment already exists, requeuing")
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "failed to create deployment")
		r.Recorder.Event(model, corev1.EventTypeWarning, "FailedCreate",
			fmt.Sprintf("Failed to create deployment: %v", err))
		return ctrl.Result{}, fmt.Errorf("failed to create deployment: %w", err)
	}

	// Allocate memory budget
	if !r.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector) {
		// This shouldn't happen since we checked with CanAllocate, but handle it
		logger.Error(nil, "failed to allocate memory after creating deployment")
		// Clean up the deployment we just created
		_ = r.Client.Delete(ctx, deployment)
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("Created deployment for InferenceModel",
		"model", model.Name,
		"namespace", model.Namespace,
		"memory_declared", model.Spec.Memory,
		"node", getNodeSelectorKey(model.Spec.NodeSelector),
		"container_image", model.Spec.ContainerImage)

	r.Recorder.Event(model, corev1.EventTypeNormal, "Created",
		fmt.Sprintf("Created deployment %s", deployment.Name))

	// Update status to indicate deployment is in progress
	if err := r.setCondition(ctx, model, ConditionTypeReady, metav1.ConditionFalse,
		ReasonDeploying, "Deployment created, waiting for pods to become ready"); err != nil {
		logger.Error(err, "failed to update status condition")
	}

	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

// buildDeployment creates a Deployment spec from an InferenceModel
func (r *InferenceModelReconciler) buildDeployment(model *inferencev1alpha1.InferenceModel) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":      model.Name,
		"app.kubernetes.io/component": "inference-server",
		"inference.eh-ops.io/model":   model.Spec.ModelName,
		"inference.eh-ops.io/managed": "true",
	}

	// Build container
	container := corev1.Container{
		Name:            "inference",
		Image:           model.Spec.ContainerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: DefaultContainerPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: []corev1.EnvVar{
			{
				Name:  "MODEL_NAME",
				Value: model.Spec.ModelName,
			},
			{
				Name:  "BACKEND_URL",
				Value: model.Spec.BackendURL,
			},
			{
				Name:  "PORT",
				Value: fmt.Sprintf("%d", DefaultContainerPort),
			},
		},
		Resources: r.buildResourceRequirements(model),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt(DefaultContainerPort),
				},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt(DefaultContainerPort),
				},
			},
			InitialDelaySeconds: 60,
			PeriodSeconds:       30,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
	}

	// Add environment variables from spec if any
	container.Env = append(container.Env, r.buildEnvVars(model)...)

	// Build pod spec
	podSpec := corev1.PodSpec{
		Containers:   []corev1.Container{container},
		NodeSelector: model.Spec.NodeSelector,
	}

	// Add tolerations for GPU nodes if needed
	if model.Spec.Resources.Limits.GPU != "" || model.Spec.Resources.Requests.GPU != "" {
		podSpec.Tolerations = []corev1.Toleration{
			{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}
	}

	// Build deployment
	replicas := model.Spec.MaxReplicas
	if replicas == 0 {
		replicas = 1
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      model.Name,
			Namespace: model.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"inference.eh-ops.io/memory": model.Spec.Memory,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": model.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						LastRequestAnnotation: time.Now().UTC().Format(time.RFC3339),
					},
				},
				Spec: podSpec,
			},
		},
	}

	return deployment
}

// buildResourceRequirements creates ResourceRequirements from the InferenceModel spec
func (r *InferenceModelReconciler) buildResourceRequirements(model *inferencev1alpha1.InferenceModel) corev1.ResourceRequirements {
	requirements := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}

	// Set requests
	if model.Spec.Resources.Requests.CPU != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Requests.CPU); err == nil {
			requirements.Requests[corev1.ResourceCPU] = q
		}
	}
	if model.Spec.Resources.Requests.Memory != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Requests.Memory); err == nil {
			requirements.Requests[corev1.ResourceMemory] = q
		}
	} else if model.Spec.Memory != "" {
		// Use the spec memory as the default memory request
		if q, err := resource.ParseQuantity(model.Spec.Memory); err == nil {
			requirements.Requests[corev1.ResourceMemory] = q
		}
	}
	if model.Spec.Resources.Requests.GPU != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Requests.GPU); err == nil {
			requirements.Requests[corev1.ResourceName("nvidia.com/gpu")] = q
		}
	}

	// Set limits
	if model.Spec.Resources.Limits.CPU != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Limits.CPU); err == nil {
			requirements.Limits[corev1.ResourceCPU] = q
		}
	}
	if model.Spec.Resources.Limits.Memory != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Limits.Memory); err == nil {
			requirements.Limits[corev1.ResourceMemory] = q
		}
	} else if model.Spec.Memory != "" {
		// Use the spec memory as the default memory limit
		if q, err := resource.ParseQuantity(model.Spec.Memory); err == nil {
			requirements.Limits[corev1.ResourceMemory] = q
		}
	}
	if model.Spec.Resources.Limits.GPU != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Limits.GPU); err == nil {
			requirements.Limits[corev1.ResourceName("nvidia.com/gpu")] = q
		}
	}

	return requirements
}

// buildEnvVars creates environment variables from the InferenceModel spec
func (r *InferenceModelReconciler) buildEnvVars(model *inferencev1alpha1.InferenceModel) []corev1.EnvVar {
	// This can be extended to support custom environment variables from the spec
	return nil
}

// handleIdleScaling checks if the model should be scaled to zero due to inactivity
func (r *InferenceModelReconciler) handleIdleScaling(ctx context.Context, model *inferencev1alpha1.InferenceModel, deployment *appsv1.Deployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Skip if already scaled to zero
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		logger.V(1).Info("Model is already scaled to zero")
		return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
	}

	// Get cooldown period (default to 10 minutes)
	cooldownPeriod := model.Spec.CooldownPeriod.Duration
	if cooldownPeriod == 0 {
		cooldownPeriod = 10 * time.Minute
	}

	// Check last request time from annotation
	lastRequestStr := deployment.Spec.Template.Annotations[LastRequestAnnotation]
	if lastRequestStr == "" {
		// No annotation yet, set it and continue
		return r.updateLastRequestAnnotation(ctx, deployment)
	}

	lastRequest, err := time.Parse(time.RFC3339, lastRequestStr)
	if err != nil {
		logger.Error(err, "failed to parse last request time, resetting")
		return r.updateLastRequestAnnotation(ctx, deployment)
	}

	// Calculate idle time
	idleTime := time.Since(lastRequest)

	// Scale to zero if idle time exceeds cooldown period
	if idleTime > cooldownPeriod {
		logger.Info("Model idle time exceeded cooldown period, scaling to zero",
			"model", model.Name, "idleTime", idleTime, "cooldownPeriod", cooldownPeriod)
		return r.scaleToZero(ctx, model, deployment)
	}

	// Requeue when we should check again
	nextCheck := cooldownPeriod - idleTime
	if nextCheck > IdleCheckInterval {
		nextCheck = IdleCheckInterval
	}

	logger.V(1).Info("Model still within cooldown period",
		"model", model.Name, "idleTime", idleTime, "cooldownPeriod", cooldownPeriod,
		"nextCheck", nextCheck)

	return ctrl.Result{RequeueAfter: nextCheck}, nil
}

// updateLastRequestAnnotation updates the last request annotation on the deployment
func (r *InferenceModelReconciler) updateLastRequestAnnotation(ctx context.Context, deployment *appsv1.Deployment) (ctrl.Result, error) {
	deploymentCopy := deployment.DeepCopy()
	if deploymentCopy.Spec.Template.Annotations == nil {
		deploymentCopy.Spec.Template.Annotations = make(map[string]string)
	}
	deploymentCopy.Spec.Template.Annotations[LastRequestAnnotation] = time.Now().UTC().Format(time.RFC3339)

	if err := r.Update(ctx, deploymentCopy); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update last request annotation: %w", err)
	}

	return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
}

// scaleToZero scales the deployment to 0 replicas
func (r *InferenceModelReconciler) scaleToZero(ctx context.Context, model *inferencev1alpha1.InferenceModel, deployment *appsv1.Deployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Scale to zero
	deploymentCopy := deployment.DeepCopy()
	zero := int32(0)
	deploymentCopy.Spec.Replicas = &zero

	if err := r.Update(ctx, deploymentCopy); err != nil {
		logger.Error(err, "failed to scale deployment to zero")
		return ctrl.Result{}, fmt.Errorf("failed to scale deployment to zero: %w", err)
	}

	logger.Info("Scaled deployment to zero replicas", "model", model.Name)
	r.Recorder.Event(model, corev1.EventTypeNormal, "ScaledToZero",
		fmt.Sprintf("Scaled deployment to 0 replicas due to inactivity"))

	// Update status
	if err := r.setCondition(ctx, model, ConditionTypeReady, metav1.ConditionFalse,
		ReasonScaledToZero, "Model scaled to zero due to inactivity"); err != nil {
		logger.Error(err, "failed to update status condition")
	}

	// Release memory budget when scaled to zero
	r.Tracker.ReleaseModel(model.Name, model.Namespace)

	return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
}

// handleUpdate checks if the deployment needs to be updated based on spec changes
func (r *InferenceModelReconciler) handleUpdate(ctx context.Context, model *inferencev1alpha1.InferenceModel, deployment *appsv1.Deployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	needsUpdate := false
	deploymentCopy := deployment.DeepCopy()

	// Check if container image changed
	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		if deployment.Spec.Template.Spec.Containers[0].Image != model.Spec.ContainerImage {
			logger.Info("Container image changed, updating deployment",
				"old", deployment.Spec.Template.Spec.Containers[0].Image,
				"new", model.Spec.ContainerImage)
			deploymentCopy.Spec.Template.Spec.Containers[0].Image = model.Spec.ContainerImage
			needsUpdate = true
		}
	}

	// Check if node selector changed
	if !mapsEqual(deployment.Spec.Template.Spec.NodeSelector, model.Spec.NodeSelector) {
		logger.Info("Node selector changed, updating deployment")
		deploymentCopy.Spec.Template.Spec.NodeSelector = model.Spec.NodeSelector

		// Re-allocate memory budget for new node selector
		if !r.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector) {
			logger.Info("Insufficient memory budget for new node selector")
			r.Recorder.Event(model, corev1.EventTypeWarning, "InsufficientMemory",
				"Cannot update node selector: insufficient memory budget on target node pool")
			return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
		}

		// Release old allocation and create new one
		r.Tracker.ReleaseModel(model.Name, model.Namespace)
		r.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector)
		needsUpdate = true
	}

	// Check if memory annotation changed
	if deployment.Annotations["inference.eh-ops.io/memory"] != model.Spec.Memory {
		if deploymentCopy.Annotations == nil {
			deploymentCopy.Annotations = make(map[string]string)
		}
		deploymentCopy.Annotations["inference.eh-ops.io/memory"] = model.Spec.Memory
		needsUpdate = true
	}

	// Note: Replicas are not automatically restored from zero here.
	// Scale-up is handled by UpdateLastRequestTime when a request comes in via the proxy,
	// or by a change in the InferenceModel spec that triggers a reconciliation.

	// Check if resources changed
	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		currentResources := deployment.Spec.Template.Spec.Containers[0].Resources
		desiredResources := r.buildResourceRequirements(model)

		if !resourceListsEqual(currentResources.Requests, desiredResources.Requests) ||
			!resourceListsEqual(currentResources.Limits, desiredResources.Limits) {
			logger.Info("Resources changed, updating deployment")
			deploymentCopy.Spec.Template.Spec.Containers[0].Resources = desiredResources
			needsUpdate = true
		}
	}

	if needsUpdate {
		if err := r.Update(ctx, deploymentCopy); err != nil {
			logger.Error(err, "failed to update deployment")
			return ctrl.Result{}, fmt.Errorf("failed to update deployment: %w", err)
		}

		logger.Info("Updated deployment for InferenceModel", "model", model.Name)
		r.Recorder.Event(model, corev1.EventTypeNormal, "Updated",
			fmt.Sprintf("Updated deployment %s", deployment.Name))

		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

// updateStatus updates the InferenceModel status based on deployment state
func (r *InferenceModelReconciler) updateStatus(ctx context.Context, model *inferencev1alpha1.InferenceModel, deployment *appsv1.Deployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if status update is needed
	oldReady := model.Status.Ready
	oldReplicas := model.Status.Replicas

	ready := deployment.Status.ReadyReplicas > 0
	model.Status.Ready = ready
	model.Status.Replicas = deployment.Status.Replicas
	model.Status.LastObservation = metav1.Now()

	// Set declared memory in status
	model.Status.DeclaredMemory = model.Spec.Memory

	// Get utilization info from tracker and update status
	utilizationInfo := r.Tracker.GetUtilization(model.Name, model.Namespace, model.Spec.Memory)
	if utilizationInfo != nil && utilizationInfo.ObservedPeak != nil {
		model.Status.ObservedPeakMemory = utilizationInfo.ObservedPeak.String()
		model.Status.UtilizationPercent = int32(utilizationInfo.Utilization * 100)
		if utilizationInfo.Recommendation != "" {
			model.Status.Recommendation = utilizationInfo.Recommendation
		}
	}

	// Determine condition status and reason
	var conditionStatus metav1.ConditionStatus
	var reason, message string

	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		conditionStatus = metav1.ConditionFalse
		reason = ReasonScaledToZero
		message = "Model is scaled to zero due to inactivity"
	} else if ready {
		conditionStatus = metav1.ConditionTrue
		reason = ReasonDeployed
		message = "Deployment is ready and serving traffic"
	} else if deployment.Status.Replicas > 0 {
		conditionStatus = metav1.ConditionFalse
		reason = ReasonDeploying
		message = fmt.Sprintf("Deployment is progressing (%d/%d replicas ready)",
			deployment.Status.ReadyReplicas, deployment.Status.Replicas)
	} else {
		conditionStatus = metav1.ConditionFalse
		reason = ReasonPending
		message = "Deployment is pending"
	}

	// Set Ready condition
	setConditionOnModel(&model.Status, ConditionTypeReady, conditionStatus, reason, message)

	// Set Available condition (based on ready replicas and updated replicas)
	if deployment.Status.ReadyReplicas > 0 && deployment.Status.UnavailableReplicas == 0 {
		setConditionOnModel(&model.Status, ConditionTypeAvailable, metav1.ConditionTrue,
			ReasonDeployed, "Model is available and serving requests")
	} else {
		setConditionOnModel(&model.Status, ConditionTypeAvailable, metav1.ConditionFalse,
			reason, message)
	}

	// Only update if status changed
	if oldReady != model.Status.Ready || oldReplicas != model.Status.Replicas || len(model.Status.Conditions) == 0 {
		if err := r.Status().Update(ctx, model); err != nil {
			if errors.IsConflict(err) {
				// Conflict is expected, requeue
				logger.V(1).Info("Conflict updating status, requeuing")
				return ctrl.Result{Requeue: true}, nil
			}
			logger.Error(err, "unable to update InferenceModel status")
			return ctrl.Result{}, err
		}
	}

	// Update metrics
	r.Metrics.UpdateModelMetrics(model)

	// Structured logging with key observability fields
	logger.V(1).Info("Updated InferenceModel status",
		"ready", ready,
		"replicas", model.Status.Replicas,
		"memory_declared", model.Spec.Memory,
		"memory_observed_peak", model.Status.ObservedPeakMemory,
		"utilization_percent", model.Status.UtilizationPercent,
		"recommendation", model.Status.Recommendation,
		"node", getNodeSelectorKey(model.Spec.NodeSelector),
	)

	// Requeue periodically to check for idle scaling
	return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
}

// getNodeSelectorKey generates a key from node selector for logging
func getNodeSelectorKey(nodeSelector map[string]string) string {
	if nodeSelector == nil {
		return "default"
	}
	// Simple implementation - use first key-value pair
	for k, v := range nodeSelector {
		return k + "=" + v
	}
	return "default"
}

// setCondition updates a condition on the model status
func (r *InferenceModelReconciler) setCondition(ctx context.Context, model *inferencev1alpha1.InferenceModel,
	conditionType string, status metav1.ConditionStatus, reason, message string) error {

	changed := setConditionOnModel(&model.Status, conditionType, status, reason, message)
	if changed {
		if err := r.Status().Update(ctx, model); err != nil {
			return err
		}
	}
	return nil
}

// setConditionOnModel sets a condition on the model status and returns true if changed
func setConditionOnModel(status *inferencev1alpha1.InferenceModelStatus,
	conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) bool {

	// Check if condition already exists with same values
	for i, cond := range status.Conditions {
		if cond.Type == conditionType {
			if cond.Status == conditionStatus && cond.Reason == reason && cond.Message == message {
				return false
			}
			// Update existing condition
			status.Conditions[i] = metav1.Condition{
				Type:               conditionType,
				Status:             conditionStatus,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: 0,
			}
			return true
		}
	}

	// Add new condition
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: 0,
	})

	return true
}

// mapsEqual checks if two string maps are equal
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// resourceListsEqual checks if two resource lists are equal
func resourceListsEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || !v.Equal(bv) {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *InferenceModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize the budget tracker if not set
	if r.Tracker == nil {
		r.Tracker = budget.NewTracker()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceModel{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

// UpdateLastRequestTime is called by the proxy when a request is made to a model
// This is a public method that can be called from the proxy handler
func (r *InferenceModelReconciler) UpdateLastRequestTime(ctx context.Context, modelName, namespace string) error {
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Name: modelName, Namespace: namespace}

	if err := r.Get(ctx, key, deployment); err != nil {
		return err
	}

	// Update the annotation
	deploymentCopy := deployment.DeepCopy()
	if deploymentCopy.Spec.Template.Annotations == nil {
		deploymentCopy.Spec.Template.Annotations = make(map[string]string)
	}
	deploymentCopy.Spec.Template.Annotations[LastRequestAnnotation] = time.Now().UTC().Format(time.RFC3339)

	// If scaled to zero, scale back up
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		// Fetch the InferenceModel to check memory budget
		model := &inferencev1alpha1.InferenceModel{}
		if err := r.Get(ctx, key, model); err != nil {
			return err
		}

		// Check if we can allocate memory
		if !r.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector) {
			return fmt.Errorf("insufficient memory budget to scale up model %s", modelName)
		}

		// Allocate memory and scale up
		r.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector)
		replicas := model.Spec.MaxReplicas
		if replicas == 0 {
			replicas = 1
		}
		deploymentCopy.Spec.Replicas = &replicas
	}

	return r.Update(ctx, deploymentCopy)
}
