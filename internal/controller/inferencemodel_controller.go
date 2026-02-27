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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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
//+kubebuilder:rbac:groups=inference.eh-ops.io,resources=inferencebackends,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

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

	// Handle HuggingFace download if needed
	if needsDownload(model) {
		return r.handleDownload(ctx, model)
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

// needsDownload returns true if the model needs to be downloaded from HuggingFace
func needsDownload(model *inferencev1alpha1.InferenceModel) bool {
	// Check if HuggingFace source is configured
	if model.Spec.Source.HuggingFace == nil {
		return false
	}

	// If download is already complete, no need to download again
	if model.Status.DownloadPhase == inferencev1alpha1.DownloadPhaseComplete {
		return false
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

// handleDownload handles the HuggingFace model download process
func (r *InferenceModelReconciler) handleDownload(ctx context.Context, model *inferencev1alpha1.InferenceModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if download has already failed
	if model.Status.DownloadPhase == inferencev1alpha1.DownloadPhaseFailed {
		logger.Info("Download previously failed, not retrying", "model", model.Name)
		return ctrl.Result{}, nil
	}

	// Check for existing download job
	jobName := getDownloadJobName(model)
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: model.Namespace}, job)

	if err != nil {
		if errors.IsNotFound(err) {
			// Job doesn't exist, need to create it
			return r.createDownloadJob(ctx, model)
		}
		logger.Error(err, "unable to fetch download Job")
		return ctrl.Result{}, fmt.Errorf("failed to fetch download job: %w", err)
	}

	// Job exists, check its status
	return r.checkDownloadJobStatus(ctx, model, job)
}

// getDownloadJobName returns the name for the download job
func getDownloadJobName(model *inferencev1alpha1.InferenceModel) string {
	return model.Name + "-download"
}

// createDownloadJob creates a new download job for the model
func (r *InferenceModelReconciler) createDownloadJob(ctx context.Context, model *inferencev1alpha1.InferenceModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	hf := model.Spec.Source.HuggingFace

	// Ensure PVC exists or create it
	pvcName, err := r.ensurePVC(ctx, model)
	if err != nil {
		logger.Error(err, "failed to ensure PVC for model download")
		return ctrl.Result{}, fmt.Errorf("failed to ensure PVC: %w", err)
	}

	// Build the download command
	modelDir := getModelDir(model)
	downloadCmd := r.buildDownloadCommand(hf, modelDir)

	// Build environment variables
	envVars := []corev1.EnvVar{
		{
			Name:  "HF_REPO",
			Value: hf.Repo,
		},
		{
			Name:  "MODEL_DIR",
			Value: modelDir,
		},
	}

	// Add HF_TOKEN from secret if specified
	if hf.TokenSecret != "" {
		secretKey := hf.TokenSecretKey
		if secretKey == "" {
			secretKey = "token"
		}
		envVars = append(envVars, corev1.EnvVar{
			Name: "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: hf.TokenSecret,
					},
					Key: secretKey,
				},
			},
		})
	}

	// Build the job
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getDownloadJobName(model),
			Namespace: model.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      model.Name,
				"app.kubernetes.io/component": "model-download",
				"inference.eh-ops.io/model":   model.Spec.ModelName,
				"inference.eh-ops.io/managed": "true",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "download",
							Image:   "python:3.11-slim",
							Command: []string{"sh", "-c", downloadCmd},
							Env:     envVars,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "model-cache",
									MountPath: "/models",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "model-cache",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(model, job, r.Scheme); err != nil {
		logger.Error(err, "failed to set controller reference on download job")
		return ctrl.Result{}, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create the job
	if err := r.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			logger.Info("Download job already exists, requeuing")
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "failed to create download job")
		r.Recorder.Event(model, corev1.EventTypeWarning, "DownloadFailed",
			fmt.Sprintf("Failed to create download job: %v", err))
		return ctrl.Result{}, fmt.Errorf("failed to create download job: %w", err)
	}

	logger.Info("Created download job for model",
		"model", model.Name,
		"repo", hf.Repo,
		"job", job.Name)

	r.Recorder.Event(model, corev1.EventTypeNormal, "DownloadStarted",
		fmt.Sprintf("Started download job for HuggingFace model %s", hf.Repo))

	// Update status to Downloading
	if err := r.updateDownloadStatus(ctx, model, inferencev1alpha1.DownloadPhaseDownloading, 0, "Download job created"); err != nil {
		logger.Error(err, "failed to update download status")
	}

	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

// checkDownloadJobStatus checks the status of a download job and updates the model status
func (r *InferenceModelReconciler) checkDownloadJobStatus(ctx context.Context, model *inferencev1alpha1.InferenceModel, job *batchv1.Job) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check job conditions
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			// Job completed successfully
			logger.Info("Download job completed successfully", "model", model.Name)

			if err := r.updateDownloadStatus(ctx, model, inferencev1alpha1.DownloadPhaseComplete, 100,
				"Model download completed"); err != nil {
				logger.Error(err, "failed to update download status")
				return ctrl.Result{}, err
			}

			r.Recorder.Event(model, corev1.EventTypeNormal, "DownloadComplete",
				"Model download completed successfully")

			// Requeue to proceed with deployment creation
			return ctrl.Result{Requeue: true}, nil
		}

		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			// Job failed
			logger.Info("Download job failed", "model", model.Name, "reason", condition.Reason, "message", condition.Message)

			if err := r.updateDownloadStatus(ctx, model, inferencev1alpha1.DownloadPhaseFailed, 0,
				fmt.Sprintf("Download failed: %s", condition.Message)); err != nil {
				logger.Error(err, "failed to update download status")
				return ctrl.Result{}, err
			}

			r.Recorder.Event(model, corev1.EventTypeWarning, "DownloadFailed",
				fmt.Sprintf("Model download failed: %s", condition.Message))

			return ctrl.Result{}, nil
		}
	}

	// Job is still running
	logger.V(1).Info("Download job still running", "model", model.Name)

	// Update status to Downloading if not already set
	if model.Status.DownloadPhase != inferencev1alpha1.DownloadPhaseDownloading {
		if err := r.updateDownloadStatus(ctx, model, inferencev1alpha1.DownloadPhaseDownloading, 0,
			"Download in progress"); err != nil {
			logger.Error(err, "failed to update download status")
		}
	}

	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

// updateDownloadStatus updates the download phase in the model status
func (r *InferenceModelReconciler) updateDownloadStatus(ctx context.Context, model *inferencev1alpha1.InferenceModel,
	phase inferencev1alpha1.DownloadPhase, progress int32, message string) error {

	model.Status.DownloadPhase = phase
	model.Status.DownloadProgress = progress
	model.Status.DownloadMessage = message

	// Set phase in status as well
	model.Status.Phase = string(phase)

	return r.Status().Update(ctx, model)
}

// ensurePVC ensures a PVC exists for model storage
func (r *InferenceModelReconciler) ensurePVC(ctx context.Context, model *inferencev1alpha1.InferenceModel) (string, error) {
	logger := log.FromContext(ctx)

	storage := model.Spec.Storage

	// If PVC is specified and create is false, just use the existing PVC
	if storage.PVC != "" && !storage.Create {
		return storage.PVC, nil
	}

	// If PVC name is specified for creation
	pvcName := storage.PVC
	if pvcName == "" {
		pvcName = model.Name + "-models"
	}

	// Check if PVC already exists
	existingPVC := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: model.Namespace}, existingPVC)
	if err == nil {
		// PVC exists
		return pvcName, nil
	}

	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("failed to check PVC: %w", err)
	}

	// Create a new PVC if requested
	if storage.Create || storage.PVC == "" {
		// Default size if not specified
		size := storage.Size
		if size == "" {
			size = "100Gi"
		}

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: model.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":      model.Name,
					"app.kubernetes.io/component": "model-storage",
					"inference.eh-ops.io/model":   model.Spec.ModelName,
					"inference.eh-ops.io/managed": "true",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(size),
					},
				},
			},
		}

		// Set storage class if specified
		if storage.StorageClass != "" {
			pvc.Spec.StorageClassName = &storage.StorageClass
		}

		// Set owner reference
		if err := controllerutil.SetControllerReference(model, pvc, r.Scheme); err != nil {
			return "", fmt.Errorf("failed to set controller reference on PVC: %w", err)
		}

		if err := r.Create(ctx, pvc); err != nil {
			if errors.IsAlreadyExists(err) {
				return pvcName, nil
			}
			return "", fmt.Errorf("failed to create PVC: %w", err)
		}

		logger.Info("Created PVC for model storage", "pvc", pvcName, "size", size)
		return pvcName, nil
	}

	// Use the specified PVC name (should exist)
	return storage.PVC, nil
}

// getModelDir returns the directory path for the model within the PVC
func getModelDir(model *inferencev1alpha1.InferenceModel) string {
	modelDir := model.Spec.Storage.ModelDir
	if modelDir == "" {
		// Default to model name, replacing any slashes
		modelDir = strings.ReplaceAll(model.Spec.ModelName, "/", "-")
	}
	return "/models/" + modelDir
}

// buildDownloadCommand builds the shell command for downloading the model
func (r *InferenceModelReconciler) buildDownloadCommand(hf *inferencev1alpha1.HuggingFaceSource, modelDir string) string {
	var cmd strings.Builder

	// Install dependencies
	cmd.WriteString("pip install huggingface_hub[cli] hf-transfer && ")
	cmd.WriteString("export HF_HUB_ENABLE_HF_TRANSFER=1 && ")

	// Create model directory
	cmd.WriteString(fmt.Sprintf("mkdir -p %s && ", modelDir))

	// Build hf download command
	cmd.WriteString(fmt.Sprintf("hf download ${HF_REPO} --local-dir %s", modelDir))

	// Add revision if specified
	if hf.Revision != "" {
		cmd.WriteString(fmt.Sprintf(" --revision %s", hf.Revision))
	}

	// Add include patterns for selective download
	if len(hf.Files) > 0 {
		cmd.WriteString(" --include ")
		for i, file := range hf.Files {
			if i > 0 {
				cmd.WriteString(",")
			}
			cmd.WriteString(file)
		}
	}

	// Create .ready file when complete
	cmd.WriteString(fmt.Sprintf(" && touch %s/.ready", modelDir))

	return cmd.String()
}

// createDeployment creates a new deployment for the InferenceModel
func (r *InferenceModelReconciler) createDeployment(ctx context.Context, model *inferencev1alpha1.InferenceModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the InferenceBackend
	backend := &inferencev1alpha1.InferenceBackend{}
	backendKey := client.ObjectKey{Name: model.Spec.Backend, Namespace: model.Namespace}
	if err := r.Get(ctx, backendKey, backend); err != nil {
		if errors.IsNotFound(err) {
			logger.Error(err, "InferenceBackend not found", "backend", model.Spec.Backend)
			r.Recorder.Event(model, corev1.EventTypeWarning, "BackendNotFound",
				fmt.Sprintf("InferenceBackend %s not found", model.Spec.Backend))
			return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
		}
		logger.Error(err, "failed to fetch InferenceBackend")
		return ctrl.Result{}, fmt.Errorf("failed to fetch InferenceBackend: %w", err)
	}

	// Check if we have enough memory budget
	if !r.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector) {
		logger.Info("Insufficient memory budget, cannot create deployment",
			"model", model.Name,
			"namespace", model.Namespace,
			"memory_declared", model.Spec.Resources.Memory,
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
	deployment, err := r.buildDeployment(model, backend)
	if err != nil {
		logger.Error(err, "failed to build deployment")
		return ctrl.Result{}, fmt.Errorf("failed to build deployment: %w", err)
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(model, deployment, r.Scheme); err != nil {
		logger.Error(err, "failed to set controller reference")
		return ctrl.Result{}, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create the deployment
	if err := r.Create(ctx, deployment); err != nil {
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
	if !r.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector) {
		// This shouldn't happen since we checked with CanAllocate, but handle it
		logger.Error(nil, "failed to allocate memory after creating deployment")
		// Clean up the deployment we just created
		_ = r.Delete(ctx, deployment)
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("Created deployment for InferenceModel",
		"model", model.Name,
		"namespace", model.Namespace,
		"memory_declared", model.Spec.Resources.Memory,
		"node", getNodeSelectorKey(model.Spec.NodeSelector),
		"backend", model.Spec.Backend,
		"container_image", deployment.Spec.Template.Spec.Containers[0].Image)

	r.Recorder.Event(model, corev1.EventTypeNormal, "Created",
		fmt.Sprintf("Created deployment %s", deployment.Name))

	// Update status to indicate deployment is in progress
	if err := r.setCondition(ctx, model, ConditionTypeReady, metav1.ConditionFalse,
		ReasonDeploying, "Deployment created, waiting for pods to become ready"); err != nil {
		logger.Error(err, "failed to update status condition")
	}

	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

// buildDeployment creates a Deployment spec from an InferenceModel and InferenceBackend
func (r *InferenceModelReconciler) buildDeployment(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) (*appsv1.Deployment, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":      model.Name,
		"app.kubernetes.io/component": "inference-server",
		"inference.eh-ops.io/model":   model.Spec.ModelName,
		"inference.eh-ops.io/backend": model.Spec.Backend,
		"inference.eh-ops.io/managed": "true",
	}

	// Determine the port to use (allow override from model)
	port := backend.Spec.Port
	if port == 0 {
		port = DefaultContainerPort
	}
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.Port != nil {
		port = *model.Spec.BackendOverrides.Port
	}

	// Determine the image to use (allow override from model)
	imageRef := &backend.Spec.Image
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.Image != nil {
		imageRef = model.Spec.BackendOverrides.Image
	}
	image := imageRef.GetImage()

	// Determine probe paths (allow override from model)
	readinessPath := backend.Spec.ReadinessPath
	if readinessPath == "" {
		readinessPath = "/health"
	}
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.ReadinessPath != nil {
		readinessPath = *model.Spec.BackendOverrides.ReadinessPath
	}

	livenessPath := backend.Spec.LivenessPath
	if livenessPath == "" {
		livenessPath = readinessPath
	}

	// Build environment variables - start with backend env, then add model-specific
	envVars := []corev1.EnvVar{
		{
			Name:  "MODEL_NAME",
			Value: model.Spec.ModelName,
		},
		{
			Name:  "BACKEND_URL",
			Value: fmt.Sprintf("http://localhost:%d", port),
		},
		{
			Name:  "PORT",
			Value: fmt.Sprintf("%d", port),
		},
	}
	// Add backend env vars
	envVars = append(envVars, backend.Spec.Env...)
	// Add model env vars (can override backend)
	envVars = append(envVars, model.Spec.Env...)

	// Build container
	container := corev1.Container{
		Name:            "inference",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env:          envVars,
		VolumeMounts: backend.Spec.VolumeMounts,
		Resources:    r.buildResourceRequirements(model, backend),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: readinessPath,
					Port: intstr.FromInt(int(port)),
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
					Path: livenessPath,
					Port: intstr.FromInt(int(port)),
				},
			},
			InitialDelaySeconds: 60,
			PeriodSeconds:       30,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
	}

	// Set command if specified
	if len(backend.Spec.Command) > 0 {
		container.Command = backend.Spec.Command
	}

	// Set args - start with backend args, then append model args
	container.Args = append(backend.Spec.Args, model.Spec.Args...)

	// Set security context if specified
	if backend.Spec.SecurityContext != nil {
		container.SecurityContext = backend.Spec.SecurityContext
	}

	// Build pod spec
	podSpec := corev1.PodSpec{
		Containers:   []corev1.Container{container},
		NodeSelector: model.Spec.NodeSelector,
		Volumes:      backend.Spec.Volumes,
	}

	// Add tolerations from model
	if len(model.Spec.Tolerations) > 0 {
		podSpec.Tolerations = model.Spec.Tolerations
	}

	// Add affinity from model
	if model.Spec.Affinity != nil {
		podSpec.Affinity = model.Spec.Affinity
	}

	// Add init containers from backend
	if len(backend.Spec.InitContainers) > 0 {
		podSpec.InitContainers = backend.Spec.InitContainers
	}

	// Add sidecar from model if specified
	if model.Spec.Sidecar != nil {
		sidecar := corev1.Container{
			Name:  model.Spec.Sidecar.Name,
			Image: model.Spec.Sidecar.Image,
			Ports: model.Spec.Sidecar.Ports,
			Args:  model.Spec.Sidecar.Args,
			Env:   model.Spec.Sidecar.Env,
		}
		if model.Spec.Sidecar.Resources != nil {
			sidecar.Resources = *model.Spec.Sidecar.Resources
		}
		if sidecar.Name == "" {
			sidecar.Name = "sidecar"
		}
		podSpec.Containers = append(podSpec.Containers, sidecar)
	}

	// Add tolerations for GPU nodes if GPU is configured
	gpuConfig := backend.Spec.GPU
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.GPU != nil {
		gpuConfig = model.Spec.BackendOverrides.GPU
	}
	if gpuConfig != nil && (gpuConfig.Exclusive || gpuConfig.Shared) {
		if len(podSpec.Tolerations) == 0 {
			podSpec.Tolerations = []corev1.Toleration{
				{
					Key:      "nvidia.com/gpu",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				},
			}
		}
	}

	// Build deployment
	replicas := model.Spec.Scaling.MaxReplicas
	if replicas == 0 {
		replicas = 1
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      model.Name,
			Namespace: model.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"inference.eh-ops.io/memory":  model.Spec.Resources.Memory,
				"inference.eh-ops.io/backend": model.Spec.Backend,
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

	return deployment, nil
}

// buildResourceRequirements creates ResourceRequirements from the InferenceModel spec and InferenceBackend
func (r *InferenceModelReconciler) buildResourceRequirements(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) corev1.ResourceRequirements {
	requirements := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}

	// Set memory request from spec
	if model.Spec.Resources.Memory != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Memory); err == nil {
			requirements.Requests[corev1.ResourceMemory] = q
		}
	}

	// Set CPU request from spec
	if model.Spec.Resources.CPU != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.CPU); err == nil {
			requirements.Requests[corev1.ResourceCPU] = q
		}
	}

	// Set memory limit (defaults to memory request if not specified)
	if model.Spec.Resources.MemoryLimit != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.MemoryLimit); err == nil {
			requirements.Limits[corev1.ResourceMemory] = q
		}
	} else if model.Spec.Resources.Memory != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Memory); err == nil {
			requirements.Limits[corev1.ResourceMemory] = q
		}
	}

	// Set GPU resources from backend configuration (can be overridden by model)
	gpuConfig := backend.Spec.GPU
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.GPU != nil {
		gpuConfig = model.Spec.BackendOverrides.GPU
	}

	if gpuConfig != nil {
		gpuResourceName := corev1.ResourceName("nvidia.com/gpu")
		if gpuConfig.ResourceName != "" {
			gpuResourceName = corev1.ResourceName(gpuConfig.ResourceName)
		} else if gpuConfig.Shared {
			gpuResourceName = corev1.ResourceName("amd.com/gpu-shared")
		}

		if gpuConfig.Exclusive || gpuConfig.Shared {
			// Set GPU to 1 for both exclusive and shared modes
			requirements.Requests[gpuResourceName] = resource.MustParse("1")
			requirements.Limits[gpuResourceName] = resource.MustParse("1")
		}
	}

	return requirements
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
		"Scaled deployment to 0 replicas due to inactivity")

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

	// Fetch the InferenceBackend to get current configuration
	backend := &inferencev1alpha1.InferenceBackend{}
	backendKey := client.ObjectKey{Name: model.Spec.Backend, Namespace: model.Namespace}
	if err := r.Get(ctx, backendKey, backend); err != nil {
		if errors.IsNotFound(err) {
			logger.Error(err, "InferenceBackend not found", "backend", model.Spec.Backend)
			return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
		}
		logger.Error(err, "failed to fetch InferenceBackend")
		return ctrl.Result{}, fmt.Errorf("failed to fetch InferenceBackend: %w", err)
	}

	needsUpdate := false
	deploymentCopy := deployment.DeepCopy()

	// Check if node selector changed
	if !mapsEqual(deployment.Spec.Template.Spec.NodeSelector, model.Spec.NodeSelector) {
		logger.Info("Node selector changed, updating deployment")
		deploymentCopy.Spec.Template.Spec.NodeSelector = model.Spec.NodeSelector

		// Re-allocate memory budget for new node selector
		if !r.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector) {
			logger.Info("Insufficient memory budget for new node selector")
			r.Recorder.Event(model, corev1.EventTypeWarning, "InsufficientMemory",
				"Cannot update node selector: insufficient memory budget on target node pool")
			return ctrl.Result{RequeueAfter: IdleCheckInterval}, nil
		}

		// Release old allocation and create new one
		r.Tracker.ReleaseModel(model.Name, model.Namespace)
		r.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector)
		needsUpdate = true
	}

	// Check if memory annotation changed
	if deployment.Annotations["inference.eh-ops.io/memory"] != model.Spec.Resources.Memory {
		if deploymentCopy.Annotations == nil {
			deploymentCopy.Annotations = make(map[string]string)
		}
		deploymentCopy.Annotations["inference.eh-ops.io/memory"] = model.Spec.Resources.Memory
		needsUpdate = true
	}

	// Note: Replicas are not automatically restored from zero here.
	// Scale-up is handled by UpdateLastRequestTime when a request comes in via the proxy,
	// or by a change in the InferenceModel spec that triggers a reconciliation.

	// Check if resources changed
	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		currentResources := deployment.Spec.Template.Spec.Containers[0].Resources
		desiredResources := r.buildResourceRequirements(model, backend)

		if !resourceListsEqual(currentResources.Requests, desiredResources.Requests) ||
			!resourceListsEqual(currentResources.Limits, desiredResources.Limits) {
			logger.Info("Resources changed, updating deployment")
			deploymentCopy.Spec.Template.Spec.Containers[0].Resources = desiredResources
			needsUpdate = true
		}
	}

	// Check if container image changed (from backend or override)
	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		imageRef := &backend.Spec.Image
		if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.Image != nil {
			imageRef = model.Spec.BackendOverrides.Image
		}
		desiredImage := imageRef.GetImage()
		if deployment.Spec.Template.Spec.Containers[0].Image != desiredImage {
			logger.Info("Container image changed, updating deployment",
				"old", deployment.Spec.Template.Spec.Containers[0].Image,
				"new", desiredImage)
			deploymentCopy.Spec.Template.Spec.Containers[0].Image = desiredImage
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
	model.Status.AvailableReplicas = deployment.Status.AvailableReplicas

	// Set declared memory in status
	model.Status.DeclaredMemory = model.Spec.Resources.Memory

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
		Owns(&batchv1.Job{}).
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
		if !r.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector) {
			return fmt.Errorf("insufficient memory budget to scale up model %s", modelName)
		}

		// Allocate memory and scale up
		r.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector)
		replicas := model.Spec.Scaling.MaxReplicas
		if replicas == 0 {
			replicas = 1
		}
		deploymentCopy.Spec.Replicas = &replicas
	}

	return r.Update(ctx, deploymentCopy)
}
