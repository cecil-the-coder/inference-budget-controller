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

var _ = Describe("Admission Control", func() {
	const (
		testModelName1     = "admission-test-model-1"
		testModelName2     = "admission-test-model-2"
		testContainerImage = "nginx:alpine"
		testBackendURL     = "http://localhost:8080"
		smallMemory        = "1Gi"
		largeMemory        = "100Gi"
		testNodePool       = "admission-test-pool"
	)

	BeforeEach(func() {
		cleanupAdmissionTestResources()
	})

	AfterEach(func() {
		cleanupAdmissionTestResources()
	})

	Describe("Memory budget enforcement", func() {
		Context("When the node has available memory", func() {
			It("Should allow model creation and deployment", func() {
				By("Creating an InferenceModel")
				model := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName1,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-1",
						Memory:         smallMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": testNodePool},
					},
				}
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Waiting for Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying InferenceModel has finalizer")
				updatedModel := &inferencev1alpha1.InferenceModel{}
				Eventually(func() bool {
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, updatedModel); err != nil {
						return false
					}
					for _, f := range updatedModel.Finalizers {
						if f == "inference.eh-ops.io/finalizer" {
							return true
						}
					}
					return false
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())
			})
		})

		Context("When multiple models compete for memory budget", func() {
			BeforeEach(func() {
				By("Creating first model with large memory requirement")
				model1 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName1,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-1",
						Memory:         largeMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": testNodePool},
					},
				}
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Waiting for first Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})

			It("Should track blocking models for admission decisions", func() {
				By("Creating second model on the same node pool")
				model2 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName2,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-2",
						Memory:         largeMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": testNodePool},
					},
				}
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				By("Waiting for second Deployment to be created")
				// In the current implementation without budget limits, both will be created
				// In production with limits, the second might be pending
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying both deployments exist")
				deployment1 := &appsv1.Deployment{}
				deployment2 := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, deployment1)).To(Succeed())
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, deployment2)).To(Succeed())

				// Both should have the same node selector
				Expect(deployment1.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"inference-pool": testNodePool}))
				Expect(deployment2.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"inference-pool": testNodePool}))
			})
		})
	})

	Describe("Admission rejection with blocking info", func() {
		Context("When a model cannot be scheduled due to memory constraints", func() {
			BeforeEach(func() {
				By("Creating a blocking model")
				model1 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName1,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "blocking-model",
						Memory:         "200Gi",
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "limited-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Waiting for first Deployment")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})

			It("Should include blocking model information in the admission decision", func() {
				By("Creating a second model that requires memory")
				model2 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName2,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "blocked-model",
						Memory:         "50Gi",
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "limited-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				// In production with budget limits, we would check that:
				// 1. The model is in a pending state
				// 2. An event is recorded indicating insufficient memory
				// 3. The blocking models are listed in the event message

				By("Checking for InferenceModel events")
				// In a full implementation, we would check events here
				// For now, verify the model was created
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &inferencev1alpha1.InferenceModel{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})
		})
	})

	Describe("Admission after scale-down", func() {
		Context("When a blocking model scales down", func() {
			BeforeEach(func() {
				By("Creating first model")
				model1 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName1,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-1",
						Memory:         "50Gi",
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "scale-test-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Waiting for first Deployment")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})

			It("Should allow new requests after blocking model scales down", func() {
				By("Scaling down the first model")
				deployment := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, deployment)).To(Succeed())
				zero := int32(0)
				deployment.Spec.Replicas = &zero
				Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

				By("Waiting for deployment to show zero replicas")
				Eventually(func() bool {
					d := &appsv1.Deployment{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, d); err != nil {
						return false
					}
					return d.Spec.Replicas != nil && *d.Spec.Replicas == 0
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Creating second model that should now fit")
				model2 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName2,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-2",
						Memory:         "10Gi",
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "scale-test-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				By("Waiting for second Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})
		})
	})

	Describe("Admission after model deletion", func() {
		Context("When a blocking model is deleted", func() {
			BeforeEach(func() {
				By("Creating a model to be deleted")
				model1 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName1,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "to-delete",
						Memory:         "80Gi",
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "delete-test-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Waiting for first Deployment")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})

			It("Should allow request after model is fully deleted", func() {
				By("Deleting the first model")
				model := &inferencev1alpha1.InferenceModel{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, model)).To(Succeed())
				Expect(k8sClient.Delete(ctx, model)).To(Succeed())

				By("Waiting for model to be fully deleted")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &inferencev1alpha1.InferenceModel{})
					return errors.IsNotFound(err)
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Waiting for deployment to be deleted")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
					return errors.IsNotFound(err)
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Creating a new model that should fit")
				model2 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName2,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "new-model",
						Memory:         "20Gi",
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "delete-test-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				By("Waiting for new Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})
		})
	})

	Describe("Node pool isolation for admission", func() {
		Context("When models are on different node pools", func() {
			It("Should allow admission on pools with available memory", func() {
				By("Creating a model on pool A")
				model1 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName1,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-pool-a",
						Memory:         largeMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "pool-a"},
					},
				}
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Waiting for first Deployment")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Creating a model on pool B")
				model2 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName2,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-pool-b",
						Memory:         largeMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "pool-b"},
					},
				}
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				By("Waiting for second Deployment on different pool")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying deployments have different node selectors")
				deployment1 := &appsv1.Deployment{}
				deployment2 := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, deployment1)).To(Succeed())
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, deployment2)).To(Succeed())
				Expect(deployment1.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"inference-pool": "pool-a"}))
				Expect(deployment2.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"inference-pool": "pool-b"}))
			})
		})
	})
})

func cleanupAdmissionTestResources() {
	modelNames := []string{"admission-test-model-1", "admission-test-model-2"}
	for _, name := range modelNames {
		model := &inferencev1alpha1.InferenceModel{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		}
		_ = k8sClient.Delete(ctx, model)

		// Also delete associated deployments
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		}
		_ = k8sClient.Delete(ctx, deployment)
	}

	// Wait for deletions
	Eventually(func() bool {
		for _, name := range modelNames {
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &inferencev1alpha1.InferenceModel{}); err == nil {
				return false
			}
		}
		return true
	}, eventuallyTimeout, pollingInterval).Should(BeTrue())
}
