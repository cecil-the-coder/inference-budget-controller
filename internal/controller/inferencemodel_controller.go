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
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
	"github.com/cecil-the-coder/inference-budget-controller/internal/budget"
	"github.com/cecil-the-coder/inference-budget-controller/internal/download"
	"github.com/cecil-the-coder/inference-budget-controller/internal/metrics"
	"github.com/cecil-the-coder/inference-budget-controller/internal/registry"
)

const (
	// FinalizerName is the finalizer used for cleanup
	FinalizerName = "inference.eh-ops.io/finalizer"

	// DefaultContainerPort is the default port for the inference container
	DefaultContainerPort = 8080

	// LastRequestAnnotation is the annotation key for tracking last request time
	LastRequestAnnotation = "inference.eh-ops.io/last-request-time"

	// RetryDownloadAnnotation is the annotation key for triggering a retry of a failed download
	RetryDownloadAnnotation = "inference.eh-ops.io/retry-download"

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
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	Tracker         *budget.Tracker
	Metrics         *metrics.Collector
	Registry        *registry.DeploymentRegistry
	DownloadManager *download.Manager
	CacheManager    *download.CacheManager
	MetricsClient   metricsv.MetricsV1beta1Interface
}

//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencemodels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencemodels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencemodels/finalizers,verbs=update
//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencebackends,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

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

	// Handle retry annotation for failed or completed downloads
	if _, retryRequested := model.Annotations[RetryDownloadAnnotation]; retryRequested {
		if model.Status.DownloadPhase == inferencev1alpha1.DownloadPhaseFailed ||
			model.Status.DownloadPhase == inferencev1alpha1.DownloadPhaseComplete {
			logger.Info("Retry annotation detected, clearing and re-downloading", "model", model.Name, "currentPhase", model.Status.DownloadPhase)

			// Cancel any existing download
			r.DownloadManager.Cancel(model.Name)

			// Remove the ready marker to force re-download
			cacheDir := r.getCacheDir(model.Name)
			if err := download.RemoveReadyMarker(cacheDir); err != nil {
				logger.Error(err, "failed to remove ready marker for retry")
			}

			// Remove the retry annotation
			delete(model.Annotations, RetryDownloadAnnotation)
			if err := r.Update(ctx, model); err != nil {
				logger.Error(err, "failed to remove retry annotation")
				return ctrl.Result{}, fmt.Errorf("failed to remove retry annotation: %w", err)
			}

			// Reset download status to pending to trigger a new download
			model.Status.DownloadPhase = inferencev1alpha1.DownloadPhasePending
			model.Status.DownloadProgress = 0
			model.Status.DownloadMessage = "Retry requested"
			model.Status.DownloadError = ""
			if err := r.Status().Update(ctx, model); err != nil {
				logger.Error(err, "failed to reset download status for retry")
				return ctrl.Result{}, fmt.Errorf("failed to reset download status: %w", err)
			}

			r.Recorder.Event(model, corev1.EventTypeNormal, "DownloadRetry",
				"Download retry triggered via annotation")

			// Requeue to start the new download
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Handle HuggingFace download if needed
	if r.needsDownload(model) {
		return r.handleDownload(ctx, model)
	}

	// Check if pod exists
	pod := &corev1.Pod{}
	podKey := client.ObjectKey{Name: req.Name, Namespace: req.Namespace}

	err := r.Get(ctx, podKey, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Pod doesn't exist - set registry state and wait for on-demand creation
			// The proxy will create the pod when a request comes in
			r.Registry.SetState(model.Namespace, model.Name, registry.StateNonexistent)

			// Update status to reflect no pod exists (scaled to zero)
			model.Status.Phase = "ScaledToZero"
			model.Status.Ready = false
			model.Status.Replicas = 0
			model.Status.AvailableReplicas = 0
			if err := r.Status().Update(ctx, model); err != nil {
				logger.Error(err, "failed to update status for nonexistent pod")
			}

			// Model is in its desired state (scaled to zero, waiting for on-demand creation)
			if err := r.setCondition(ctx, model, ConditionTypeReady, metav1.ConditionTrue,
				ReasonScaledToZero, "Scaled to zero, waiting for first request"); err != nil {
				logger.Error(err, "failed to update status condition")
			}

			// Requeue periodically to check for changes
			return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
		}
		logger.Error(err, "unable to fetch Pod")
		return ctrl.Result{}, err
	}

	// Check for scale-to-zero based on idle time
	if result, err := r.handleIdleScaling(ctx, model, pod); err != nil {
		return result, err
	} else if result.Requeue || result.RequeueAfter > 0 {
		return result, nil
	}

	// Update status based on pod state
	return r.updateStatus(ctx, model, pod)
}

// needsDownload returns true if the model needs to be downloaded from HuggingFace.
func (r *InferenceModelReconciler) needsDownload(model *inferencev1alpha1.InferenceModel) bool {
	// Check if HuggingFace source is configured
	if model.Spec.Source.HuggingFace == nil {
		return false
	}

	// If download is already complete, just check if the ready marker exists.
	// Full verification is done in handleDownload which also handles cache cleanup.
	if model.Status.DownloadPhase == inferencev1alpha1.DownloadPhaseComplete {
		cacheDir := filepath.Join("/models", model.Name)
		return !download.CheckReadyMarker(cacheDir)
	}

	return true
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
		"memory_declared", model.Spec.Resources.Memory,
		"utilization_percent", model.Status.UtilizationPercent,
	)

	// Release memory budget
	r.Tracker.ReleaseModel(model.Name, model.Namespace)

	// Clean up metrics
	r.Metrics.DeleteModelMetrics(model.Name, model.Namespace)

	// Remove from registry
	r.Registry.Delete(model.Namespace, model.Name)

	// Note: Model files on shared PVCs are not automatically cleaned up.
	// If cleanup is needed, it should be done manually or via a separate process.

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

// handleDownload handles the HuggingFace model download process using DownloadManager
func (r *InferenceModelReconciler) handleDownload(ctx context.Context, model *inferencev1alpha1.InferenceModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if model is already cached (ready marker exists AND files verify)
	cacheDir := r.getCacheDir(model.Name)
	verifyResult := download.CheckAndVerifyReadyMarkerDetailed(cacheDir)
	if verifyResult.Verified {
		model.Status.DownloadPhase = inferencev1alpha1.DownloadPhaseComplete
		if err := r.Status().Update(ctx, model); err != nil {
			return ctrl.Result{}, err
		}
		return r.transitionToDeploying(ctx, model)
	}

	// Only clean HF cache when marker existed but verification failed (corruption),
	// not on first download when there's no marker yet
	if verifyResult.MarkerExists && model.Spec.Source.HuggingFace != nil {
		repoID := model.Spec.Source.HuggingFace.Repo
		hfHome := os.Getenv("HF_HOME")
		if hfHome == "" {
			hfHome = "/models" // default
		}
		logger.Info("Verification failed, cleaning HuggingFace cache", "repo", repoID, "error", verifyResult.Error)
		if err := download.CleanHuggingFaceCache(hfHome, repoID); err != nil {
			logger.Error(err, "Failed to clean HuggingFace cache", "repo", repoID)
		}
	}

	// Check existing download status
	status := r.DownloadManager.GetStatus(model.Name)

	// Start a new download only if nothing is tracked
	if status == nil {
		logger.Info("Starting model download", "model", model.Name)
		r.Recorder.Event(model, "Normal", "DownloadStarted", "Starting model download")

		spec := r.buildDownloadSpec(model)
		if err := r.DownloadManager.Download(ctx, model.Name, spec); err != nil {
			logger.V(1).Info("Download already in progress", "model", model.Name, "err", err)
		}

		status = r.DownloadManager.GetStatus(model.Name)
	}

	if status == nil {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Update CRD status
	phase := status.GetPhase()
	logger.V(1).Info("Download status check", "model", model.Name, "phase", phase, "progress", status.GetProgress())
	model.Status.DownloadPhase = convertDownloadPhase(phase)
	model.Status.DownloadProgress = int32(status.Progress)
	model.Status.DownloadBytesTotal = status.BytesTotal
	model.Status.DownloadBytesDone = status.BytesDone
	model.Status.DownloadSpeed = status.GetBytesDone() / int64(status.Duration().Seconds()+1)

	// Get progress snapshot for ETA
	snapshot := r.getProgressSnapshot(status)
	if snapshot.ETA > 0 {
		model.Status.DownloadETA = formatDuration(snapshot.ETA)
	}

	if err := r.Status().Update(ctx, model); err != nil {
		return ctrl.Result{}, err
	}

	// Handle completion/failure
	switch phase {
	case download.PhaseComplete:
		logger.Info("Model download complete", "model", model.Name)
		r.Recorder.Event(model, "Normal", "DownloadComplete",
			fmt.Sprintf("Downloaded %d bytes in %s", status.BytesDone, status.Duration()))

		// Write ready marker then clean up download status
		if err := download.WriteReadyMarker(cacheDir); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to write ready marker: %w", err)
		}
		r.DownloadManager.RemoveStatus(model.Name)

		return r.transitionToDeploying(ctx, model)

	case download.PhaseFailed:
		err := fmt.Errorf("download failed: %s", status.Error)
		logger.Error(err, "Model download failed", "model", model.Name)
		r.Recorder.Event(model, "Warning", "DownloadFailed", status.Error)
		model.Status.DownloadError = status.Error
		r.DownloadManager.RemoveStatus(model.Name)
		return ctrl.Result{}, err

	default:
		// Still downloading, requeue
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
}

// buildDownloadSpec builds a DownloadSpec from the InferenceModel CRD
func (r *InferenceModelReconciler) buildDownloadSpec(model *inferencev1alpha1.InferenceModel) *download.DownloadSpec {
	hf := model.Spec.Source.HuggingFace

	// Use revision as branch if specified
	branch := hf.Revision
	if branch == "" {
		branch = "main"
	}

	// Separate exact files from glob patterns
	// Files containing '*' or '?' are treated as glob patterns
	var exactFiles []string
	var patterns []string
	for _, f := range hf.Files {
		if strings.Contains(f, "*") || strings.Contains(f, "?") {
			patterns = append(patterns, f)
		} else {
			exactFiles = append(exactFiles, f)
		}
	}

	return download.NewDownloadSpec(hf.Repo,
		download.WithBranch(branch),
		download.WithFiles(exactFiles...),
		download.WithPatterns(patterns...),
		download.WithDestDir(r.getCacheDir(model.Name)),
	)
}

// getCacheDir returns the cache directory for a model
func (r *InferenceModelReconciler) getCacheDir(modelName string) string {
	return filepath.Join("/models", modelName)
}

// transitionToDeploying transitions the model to the deploying phase
func (r *InferenceModelReconciler) transitionToDeploying(ctx context.Context, model *inferencev1alpha1.InferenceModel) (ctrl.Result, error) {
	// Update status and move to next phase
	model.Status.Phase = "Deploying"
	if err := r.Status().Update(ctx, model); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// convertDownloadPhase converts download.DownloadPhase to inferencev1alpha1.DownloadPhase
func convertDownloadPhase(p download.DownloadPhase) inferencev1alpha1.DownloadPhase {
	switch p {
	case download.PhasePending:
		return inferencev1alpha1.DownloadPhasePending
	case download.PhaseDownloading:
		return inferencev1alpha1.DownloadPhaseDownloading
	case download.PhaseComplete:
		return inferencev1alpha1.DownloadPhaseComplete
	case download.PhaseFailed:
		return inferencev1alpha1.DownloadPhaseFailed
	default:
		return inferencev1alpha1.DownloadPhasePending
	}
}

// formatDuration formats a duration for human-readable display
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}

	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// getProgressSnapshot gets a progress snapshot from the download status
// This is a helper to get ETA information
func (r *InferenceModelReconciler) getProgressSnapshot(status *download.Status) download.ProgressSnapshot {
	// Create a basic snapshot from the status
	// Note: For more accurate ETA, the download package would need to expose
	// the ProgressTracker's Snapshot method
	return download.ProgressSnapshot{
		BytesCompleted: status.BytesDone,
		BytesTotal:     status.BytesTotal,
		Percentage:     status.Progress,
		Elapsed:        status.Duration(),
	}
}

// handleIdleScaling checks if the model should be scaled to zero due to inactivity
func (r *InferenceModelReconciler) handleIdleScaling(ctx context.Context, model *inferencev1alpha1.InferenceModel, pod *corev1.Pod) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// If scaling is disabled, don't do idle scaling
	scalingDisabled := model.Spec.Scaling.Enabled != nil && !*model.Spec.Scaling.Enabled
	if scalingDisabled {
		return ctrl.Result{}, nil
	}

	// If eviction policy is MemoryPressureOnly, skip idle scaling
	if model.Spec.Scaling.EvictionPolicy == inferencev1alpha1.EvictionPolicyMemoryPressureOnly {
		return ctrl.Result{}, nil
	}

	// Check if there are active requests (from registry)
	entry := r.Registry.Get(model.Namespace, model.Name)
	if entry != nil && entry.ActiveRequests > 0 {
		logger.V(1).Info("Model has active requests, skipping idle check",
			"model", model.Name, "active_requests", entry.ActiveRequests)
		return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
	}

	// Get cooldown period (default to 5 minutes)
	var cooldownPeriod time.Duration
	if model.Spec.Scaling.ScaleToZeroDelay != "" {
		if d, err := time.ParseDuration(model.Spec.Scaling.ScaleToZeroDelay); err == nil {
			cooldownPeriod = d
		}
	}
	if cooldownPeriod == 0 && model.Spec.Scaling.CooldownPeriod != "" {
		if d, err := time.ParseDuration(model.Spec.Scaling.CooldownPeriod); err == nil {
			cooldownPeriod = d
		}
	}
	if cooldownPeriod == 0 {
		cooldownPeriod = 5 * time.Minute
	}

	// Check last request time from annotation
	lastRequestStr := pod.Annotations[LastRequestAnnotation]
	if lastRequestStr == "" {
		// No annotation yet, set it and continue
		return r.updateLastRequestAnnotation(ctx, pod)
	}

	lastRequest, err := time.Parse(time.RFC3339, lastRequestStr)
	if err != nil {
		logger.Error(err, "failed to parse last request time, resetting")
		return r.updateLastRequestAnnotation(ctx, pod)
	}

	// Calculate idle time
	idleTime := time.Since(lastRequest)

	// Delete pod if idle time exceeds cooldown period
	if idleTime > cooldownPeriod {
		logger.Info("Model idle time exceeded cooldown period, deleting pod",
			"model", model.Name, "idleTime", idleTime, "cooldownPeriod", cooldownPeriod)
		return r.deleteIdlePod(ctx, model, pod)
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

// updateLastRequestAnnotation updates the last request annotation on the pod
func (r *InferenceModelReconciler) updateLastRequestAnnotation(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	podCopy := pod.DeepCopy()
	if podCopy.Annotations == nil {
		podCopy.Annotations = make(map[string]string)
	}
	podCopy.Annotations[LastRequestAnnotation] = time.Now().UTC().Format(time.RFC3339)

	if err := r.Update(ctx, podCopy); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update last request annotation: %w", err)
	}

	return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
}

// deleteIdlePod deletes the pod when the model is idle
func (r *InferenceModelReconciler) deleteIdlePod(ctx context.Context, model *inferencev1alpha1.InferenceModel, pod *corev1.Pod) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if there are any active requests in the registry (best-effort check)
	if r.Registry != nil {
		entry := r.Registry.Get(model.Namespace, model.Name)
		if entry != nil && entry.ActiveRequests > 0 {
			logger.Info("Model has active requests, skipping deletion",
				"model", model.Name, "activeRequests", entry.ActiveRequests)
			return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
		}
	}

	// Check if the model is already being scaled down by another reconcile loop
	// This acts as a distributed lock to prevent multiple concurrent deletions
	if isScaledDown(model) {
		logger.Info("Model is already being scaled down, skipping", "model", model.Name)
		return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
	}

	// Set the "ScaledToZero" condition FIRST as a distributed lock
	// This prevents other reconcile loops from proceeding with deletion
	if err := r.setCondition(ctx, model, ConditionTypeReady, metav1.ConditionFalse,
		ReasonScaledToZero, "Model pod deleted due to inactivity"); err != nil {
		logger.Error(err, "failed to set scaled-to-zero condition")
		return ctrl.Result{}, fmt.Errorf("failed to set scaled-to-zero condition: %w", err)
	}

	// Now delete the pod
	if err := r.Delete(ctx, pod); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Pod already deleted", "model", model.Name, "pod", pod.Name)
		} else {
			logger.Error(err, "failed to delete pod for idle model")
			return ctrl.Result{}, fmt.Errorf("failed to delete pod for idle model: %w", err)
		}
	} else {
		logger.Info("Deleted pod for idle model", "model", model.Name, "pod", pod.Name)
		r.Recorder.Event(model, corev1.EventTypeNormal, "PodDeleted",
			fmt.Sprintf("Deleted pod %s due to inactivity", pod.Name))
	}

	// Remove from registry (idempotent)
	if r.Registry != nil {
		r.Registry.Delete(model.Namespace, model.Name)
		logger.Info("Removed model from registry", "model", model.Name)
	}

	// Release memory budget (idempotent)
	r.Tracker.ReleaseModel(model.Name, model.Namespace)

	return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
}

// isScaledDown checks if the model is already in scaled-to-zero state
func isScaledDown(model *inferencev1alpha1.InferenceModel) bool {
	for _, cond := range model.Status.Conditions {
		if cond.Type == ConditionTypeReady {
			return cond.Status == metav1.ConditionFalse && cond.Reason == ReasonScaledToZero
		}
	}
	return false
}

// updateStatus updates the InferenceModel status based on pod state
func (r *InferenceModelReconciler) updateStatus(ctx context.Context, model *inferencev1alpha1.InferenceModel, pod *corev1.Pod) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if status update is needed
	oldReady := model.Status.Ready
	oldPeak := model.Status.ObservedPeakMemory

	// Check if pod is ready (has PodReady condition true)
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
			break
		}
	}

	model.Status.Ready = ready
	model.Status.Replicas = 1 // Single pod
	model.Status.AvailableReplicas = 1

	// Set declared memory in status
	model.Status.DeclaredMemory = model.Spec.Resources.Memory

	// Query pod metrics from metrics-server and record observed memory usage
	if ready && r.MetricsClient != nil {
		podMetrics, err := r.MetricsClient.PodMetricses(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err == nil {
			for _, c := range podMetrics.Containers {
				if mem, ok := c.Usage[corev1.ResourceMemory]; ok {
					r.Tracker.RecordUsage(model.Name, model.Namespace, &mem)
					break
				}
			}
		} else {
			logger.V(1).Info("Failed to get pod metrics", "pod", pod.Name, "error", err)
		}
	}

	// Get utilization info from tracker and update status
	utilizationInfo := r.Tracker.GetUtilization(model.Name, model.Namespace, model.Spec.Resources.Memory)
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

	if ready {
		conditionStatus = metav1.ConditionTrue
		reason = ReasonDeployed
		message = "Pod is ready and serving traffic"
	} else if pod.Status.Phase == corev1.PodRunning {
		conditionStatus = metav1.ConditionFalse
		reason = ReasonDeploying
		message = "Pod is running but not yet ready"
	} else {
		conditionStatus = metav1.ConditionFalse
		reason = ReasonPending
		message = fmt.Sprintf("Pod is %s", pod.Status.Phase)
	}

	// Set Ready condition
	setConditionOnModel(&model.Status, ConditionTypeReady, conditionStatus, reason, message)

	// Set Available condition
	if ready {
		setConditionOnModel(&model.Status, ConditionTypeAvailable, metav1.ConditionTrue,
			ReasonDeployed, "Model is available and serving requests")
		// Update registry to indicate pod is ready
		r.Registry.SetState(model.Namespace, model.Name, registry.StateReady)
	} else {
		setConditionOnModel(&model.Status, ConditionTypeAvailable, metav1.ConditionFalse,
			reason, message)
	}

	// Only update if status changed
	if oldReady != model.Status.Ready || len(model.Status.Conditions) == 0 || oldPeak != model.Status.ObservedPeakMemory {
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
		"phase", pod.Status.Phase,
		"memory_declared", model.Spec.Resources.Memory,
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

// findModelsForBackend finds all InferenceModels that reference a given InferenceBackend
// and returns reconcile requests for each matching model.
func (r *InferenceModelReconciler) findModelsForBackend(ctx context.Context, obj client.Object) []reconcile.Request {
	backend, ok := obj.(*inferencev1alpha1.InferenceBackend)
	if !ok {
		return nil
	}

	// List all InferenceModels in the same namespace as the backend
	modelList := &inferencev1alpha1.InferenceModelList{}
	if err := r.List(ctx, modelList, client.InNamespace(backend.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "failed to list InferenceModels for backend watch", "backend", backend.Name)
		return nil
	}

	var requests []reconcile.Request
	for _, model := range modelList.Items {
		// Check if this model references the backend
		if model.Spec.Backend == backend.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      model.Name,
					Namespace: model.Namespace,
				},
			})
		}
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *InferenceModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize the budget tracker if not set
	if r.Tracker == nil {
		r.Tracker = budget.NewTracker()
	}

	// Create a rate limiter that allows bursts but limits overall rate
	// This prevents OOM when many models are reconciled simultaneously on startup
	rateLimiter := workqueue.NewWithMaxWaitRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(time.Second, 30*time.Second),
		60*time.Second, // max wait time
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceModel{}).
		Owns(&corev1.Pod{}).
		Watches(
			&inferencev1alpha1.InferenceBackend{},
			handler.EnqueueRequestsFromMapFunc(r.findModelsForBackend),
		).
		WithOptions(controller.Options{
			RateLimiter: rateLimiter,
		}).
		Complete(r)
}

// UpdateLastRequestTime is called by the proxy when a request is made to a model
// This is a public method that can be called from the proxy handler
func (r *InferenceModelReconciler) UpdateLastRequestTime(ctx context.Context, modelName, namespace string) error {
	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: modelName, Namespace: namespace}

	if err := r.Get(ctx, key, pod); err != nil {
		return err
	}

	// Update the annotation
	podCopy := pod.DeepCopy()
	if podCopy.Annotations == nil {
		podCopy.Annotations = make(map[string]string)
	}
	podCopy.Annotations[LastRequestAnnotation] = time.Now().UTC().Format(time.RFC3339)

	return r.Update(ctx, podCopy)
}
