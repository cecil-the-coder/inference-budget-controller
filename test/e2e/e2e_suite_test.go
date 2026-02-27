//go:build e2e
// +build e2e

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

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
)

const (
	kindClusterName   = "inference-test"
	controllerImage   = "ghcr.io/cecil-the-coder/inference-budget-controller:e2e-test"
	namespace         = "inference-system"
	pollingInterval   = 2 * time.Second
	eventuallyTimeout = 5 * time.Minute
)

var (
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc
	schemeRef *runtime.Scheme
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.Background())

	By("Setting up the scheme")
	schemeRef = runtime.NewScheme()
	Expect(scheme.AddToScheme(schemeRef)).To(Succeed())
	Expect(inferencev1alpha1.AddToScheme(schemeRef)).To(Succeed())

	By("Getting Kubernetes config")
	homeDir, err := os.UserHomeDir()
	Expect(err).NotTo(HaveOccurred())
	kubeconfigPath := filepath.Join(homeDir, ".kube", "config")

	By("Creating Kubernetes client")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(config, client.Options{Scheme: schemeRef})
	Expect(err).NotTo(HaveOccurred())

	By("Creating namespace if it doesn't exist")
	createNamespace(namespace)

	By("Building the controller image")
	buildControllerImage()

	By("Loading the controller image into kind")
	loadImageIntoKind()

	By("Installing CRDs")
	installCRDs()

	By("Deploying the controller")
	deployController()

	By("Waiting for the controller to be ready")
	waitForControllerReady()
})

var _ = AfterSuite(func() {
	By("Cleaning up the controller deployment")
	cleanupController()

	if cancel != nil {
		cancel()
	}
})

func createNamespace(name string) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

func buildControllerImage() {
	By("Building Docker image")
	projectRoot := getProjectRoot()
	cmd := exec.Command("docker", "build", "-t", controllerImage, projectRoot)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed())
}

func loadImageIntoKind() {
	By("Loading image into kind cluster")
	cmd := exec.Command("kind", "load", "docker-image", controllerImage, "--name", kindClusterName)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed())
}

func installCRDs() {
	By("Installing CRDs using kustomize")
	projectRoot := getProjectRoot()
	cmd := exec.Command("kustomize", "build", filepath.Join(projectRoot, "config", "crd"))
	output, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())

	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(output)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed())

	By("Waiting for CRDs to be established")
	Eventually(func() error {
		cmd := exec.Command("kubectl", "wait", "--for", "condition=established", "--timeout=60s",
			"crd/inferencemodels.inference.eh-ops.io")
		cmd.Stdout = GinkgoWriter
		cmd.Stderr = GinkgoWriter
		return cmd.Run()
	}, eventuallyTimeout, pollingInterval).Should(Succeed())
}

func deployController() {
	By("Deploying controller to the cluster")
	projectRoot := getProjectRoot()

	// Create manager deployment and RBAC using kustomize
	cmd := exec.Command("kustomize", "build", filepath.Join(projectRoot, "config", "default"))
	output, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())

	// Replace image tag
	deploymentYAML := strings.ReplaceAll(string(output), "controller:latest", controllerImage)

	cmd = exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
	cmd.Stdin = strings.NewReader(deploymentYAML)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed())
}

func waitForControllerReady() {
	By("Waiting for controller deployment to be ready")
	Eventually(func() error {
		cmd := exec.Command("kubectl", "rollout", "status", "deployment/inference-budget-controller-manager",
			"-n", namespace, "--timeout=120s")
		cmd.Stdout = GinkgoWriter
		cmd.Stderr = GinkgoWriter
		return cmd.Run()
	}, eventuallyTimeout, pollingInterval).Should(Succeed())
}

func cleanupController() {
	By("Deleting controller deployment")
	cmd := exec.Command("kubectl", "delete", "deployment", "-n", namespace, "-l", "control-plane=inference-budget-controller-manager")
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	_ = cmd.Run() // Ignore errors if already deleted
}

func getProjectRoot() string {
	// Get the project root by looking for go.mod
	cwd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())

	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	Fail("Could not find project root (go.mod)")
	return ""
}

// Helper function to create a test InferenceModel
func createTestInferenceModel(name, modelName, memory, backendURL, containerImage string) *inferencev1alpha1.InferenceModel {
	return &inferencev1alpha1.InferenceModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: inferencev1alpha1.InferenceModelSpec{
			ModelName:      modelName,
			Memory:         memory,
			BackendURL:     backendURL,
			ContainerImage: containerImage,
			CooldownPeriod: metav1.Duration{Duration: 1 * time.Minute},
			MaxReplicas:    1,
			NodeSelector:   map[string]string{"inference-pool": "default"},
		},
	}
}

// Helper function to wait for InferenceModel to be ready
func waitForInferenceModelReady(name string) {
	Eventually(func() bool {
		model := &inferencev1alpha1.InferenceModel{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, model)
		if err != nil {
			return false
		}
		return model.Status.Ready
	}, eventuallyTimeout, pollingInterval).Should(BeTrue())
}

// Helper function to delete an InferenceModel
func deleteInferenceModel(name string) {
	model := &inferencev1alpha1.InferenceModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	_ = k8sClient.Delete(ctx, model)

	// Wait for deletion
	Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, model)
		return errors.IsNotFound(err)
	}, eventuallyTimeout, pollingInterval).Should(BeTrue())
}
