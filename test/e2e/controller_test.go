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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
)

var _ = Describe("InferenceModel Controller", func() {
	const (
		testModelName      = "test-model"
		testContainerImage = "nginx:alpine" // Simple image for testing
		testBackendURL     = "http://localhost:8080"
		testMemory         = "1Gi"
	)

	BeforeEach(func() {
		// Clean up any existing test resources
		cleanupTestResources()
	})

	AfterEach(func() {
		// Clean up test resources after each test
		cleanupTestResources()
	})

	Describe("InferenceModel creation", func() {
		Context("When a valid InferenceModel is created", func() {
			It("Should create a Deployment and update status to Ready", func() {
				By("Creating an InferenceModel")
				model := createTestInferenceModel(testModelName, "gpt-test", testMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Waiting for the Deployment to be created")
				Eventually(func() error {
					deployment := &appsv1.Deployment{}
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, deployment)
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying the Deployment has correct labels")
				deployment := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, deployment)).To(Succeed())
				Expect(deployment.Labels["app.kubernetes.io/name"]).To(Equal(testModelName))
				Expect(deployment.Labels["inference.eh-ops.io/managed"]).To(Equal("true"))

				By("Waiting for the InferenceModel status to be updated")
				Eventually(func() bool {
					updatedModel := &inferencev1alpha1.InferenceModel{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, updatedModel); err != nil {
						return false
					}
					return updatedModel.Status.Replicas > 0
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Verifying the InferenceModel has a finalizer")
				updatedModel := &inferencev1alpha1.InferenceModel{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, updatedModel)).To(Succeed())
				Expect(updatedModel.Finalizers).To(ContainElement("inference.eh-ops.io/finalizer"))
			})
		})
	})

	Describe("InferenceModel status updates", func() {
		Context("When the Deployment becomes ready", func() {
			It("Should update the InferenceModel status to Ready=true", func() {
				By("Creating an InferenceModel")
				model := createTestInferenceModel(testModelName, "gpt-test", testMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Simulating deployment becoming ready by updating deployment status")
				Eventually(func() error {
					deployment := &appsv1.Deployment{}
					err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, deployment)
					if err != nil {
						return err
					}
					deployment.Status.ReadyReplicas = 1
					deployment.Status.Replicas = 1
					deployment.Status.AvailableReplicas = 1
					deployment.Status.UpdatedReplicas = 1
					return k8sClient.Status().Update(ctx, deployment)
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Waiting for InferenceModel status to show Ready=true")
				Eventually(func() bool {
					updatedModel := &inferencev1alpha1.InferenceModel{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, updatedModel); err != nil {
						return false
					}
					return updatedModel.Status.Ready
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Verifying status conditions")
				updatedModel := &inferencev1alpha1.InferenceModel{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, updatedModel)).To(Succeed())
				Expect(updatedModel.Status.Conditions).NotTo(BeEmpty())

				// Check Ready condition
				var readyCondition *metav1.Condition
				for i := range updatedModel.Status.Conditions {
					if updatedModel.Status.Conditions[i].Type == "Ready" {
						readyCondition = &updatedModel.Status.Conditions[i]
						break
					}
				}
				Expect(readyCondition).NotTo(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			})
		})
	})

	Describe("InferenceModel deletion with finalizer", func() {
		Context("When an InferenceModel is deleted", func() {
			It("Should handle deletion gracefully with finalizer cleanup", func() {
				By("Creating an InferenceModel")
				model := createTestInferenceModel(testModelName, "gpt-test", testMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Waiting for finalizer to be added")
				Eventually(func() bool {
					updatedModel := &inferencev1alpha1.InferenceModel{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, updatedModel); err != nil {
						return false
					}
					for _, f := range updatedModel.Finalizers {
						if f == "inference.eh-ops.io/finalizer" {
							return true
						}
					}
					return false
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Deleting the InferenceModel")
				Expect(k8sClient.Delete(ctx, model)).To(Succeed())

				By("Waiting for the InferenceModel to be fully deleted")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, &inferencev1alpha1.InferenceModel{})
					return errors.IsNotFound(err)
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Verifying the Deployment is also deleted (owned resource)")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, &appsv1.Deployment{})
					return errors.IsNotFound(err)
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())
			})
		})
	})

	Describe("Scale to zero on idle", func() {
		Context("When an InferenceModel is idle beyond cooldown period", func() {
			It("Should scale the Deployment to zero replicas", func() {
				By("Creating an InferenceModel with short cooldown")
				model := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "gpt-test",
						Memory:         testMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 5 * time.Second}, // Very short for testing
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "default"},
					},
				}
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Waiting for Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Waiting for cooldown period to elapse and scale to zero")
				// Note: In a real test, we might need to manipulate the last request annotation
				// to simulate idle time passing
				Eventually(func() bool {
					deployment := &appsv1.Deployment{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, deployment); err != nil {
						return false
					}
					return deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0
				}, 2*time.Minute, pollingInterval).Should(BeTrue())

				By("Verifying InferenceModel status shows scaled to zero")
				updatedModel := &inferencev1alpha1.InferenceModel{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, updatedModel)).To(Succeed())
				Expect(updatedModel.Status.Ready).To(BeFalse())
			})
		})
	})

	Describe("InferenceModel spec updates", func() {
		Context("When the InferenceModel spec is updated", func() {
			It("Should update the Deployment with new container image", func() {
				By("Creating an InferenceModel")
				model := createTestInferenceModel(testModelName, "gpt-test", testMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Waiting for Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Updating the InferenceModel container image")
				updatedModel := &inferencev1alpha1.InferenceModel{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, updatedModel)).To(Succeed())
				updatedModel.Spec.ContainerImage = "nginx:latest"
				Expect(k8sClient.Update(ctx, updatedModel)).To(Succeed())

				By("Waiting for Deployment to be updated with new image")
				Eventually(func() bool {
					deployment := &appsv1.Deployment{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName}, deployment); err != nil {
						return false
					}
					if len(deployment.Spec.Template.Spec.Containers) == 0 {
						return false
					}
					return deployment.Spec.Template.Spec.Containers[0].Image == "nginx:latest"
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())
			})
		})
	})
})

func cleanupTestResources() {
	// Delete InferenceModels
	modelList := &inferencev1alpha1.InferenceModelList{}
	if err := k8sClient.List(ctx, modelList, client.InNamespace(namespace)); err == nil {
		for _, model := range modelList.Items {
			_ = k8sClient.Delete(ctx, &model)
		}
	}

	// Wait for deletions to complete
	Eventually(func() bool {
		if err := k8sClient.List(ctx, modelList, client.InNamespace(namespace)); err != nil {
			return true
		}
		return len(modelList.Items) == 0
	}, eventuallyTimeout, pollingInterval).Should(BeTrue())
}
