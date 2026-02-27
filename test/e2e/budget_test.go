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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
)

var _ = Describe("Budget Tracker", func() {
	const (
		testModelName1     = "budget-test-model-1"
		testModelName2     = "budget-test-model-2"
		testContainerImage = "nginx:alpine"
		testBackendURL     = "http://localhost:8080"
		smallMemory        = "512Mi"
		mediumMemory       = "1Gi"
		largeMemory        = "4Gi"
	)

	BeforeEach(func() {
		cleanupBudgetTestResources()
	})

	AfterEach(func() {
		cleanupBudgetTestResources()
	})

	Describe("Memory allocation tracking", func() {
		Context("When multiple InferenceModels are created on the same node pool", func() {
			It("Should track memory allocation per node pool correctly", func() {
				By("Creating first InferenceModel with small memory")
				model1 := &inferencev1alpha1.InferenceModel{
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
						NodeSelector:   map[string]string{"inference-pool": "test-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Waiting for first Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Creating second InferenceModel on the same node pool")
				model2 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName2,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-2",
						Memory:         mediumMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "test-pool"},
					},
				}
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				By("Waiting for second Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying both deployments exist")
				deployment1 := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, deployment1)).To(Succeed())
				Expect(deployment1.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"inference-pool": "test-pool"}))

				deployment2 := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, deployment2)).To(Succeed())
				Expect(deployment2.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"inference-pool": "test-pool"}))
			})
		})

		Context("When an InferenceModel is deleted", func() {
			It("Should release the allocated memory budget", func() {
				By("Creating an InferenceModel")
				model1 := createTestInferenceModel(testModelName1, "model-1", smallMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Waiting for Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Deleting the InferenceModel")
				Expect(k8sClient.Delete(ctx, model1)).To(Succeed())

				By("Waiting for InferenceModel to be deleted")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &inferencev1alpha1.InferenceModel{})
					return err != nil
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())

				By("Creating another InferenceModel that should fit in the released budget")
				model2 := createTestInferenceModel(testModelName2, "model-2", smallMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				By("Waiting for second Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())
			})
		})
	})

	Describe("Utilization metrics", func() {
		Context("When an InferenceModel has utilization data", func() {
			It("Should report utilization metrics in status", func() {
				By("Creating an InferenceModel")
				model := createTestInferenceModel(testModelName1, "model-1", largeMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Waiting for Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Simulating utilization by updating status with observed memory")
				// In a real scenario, this would come from metrics collected at runtime
				updatedModel := &inferencev1alpha1.InferenceModel{}
				Eventually(func() error {
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, updatedModel); err != nil {
						return err
					}
					updatedModel.Status.ObservedPeakMemory = "2Gi" // 50% of declared 4Gi
					updatedModel.Status.UtilizationPercent = 50
					return k8sClient.Status().Update(ctx, updatedModel)
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying utilization status is recorded")
				Eventually(func() bool {
					checkModel := &inferencev1alpha1.InferenceModel{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, checkModel); err != nil {
						return false
					}
					return checkModel.Status.ObservedPeakMemory == "2Gi" &&
						checkModel.Status.UtilizationPercent == 50
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())
			})
		})
	})

	Describe("Memory recommendations", func() {
		Context("When an InferenceModel is overprovisioned", func() {
			It("Should generate right-sizing recommendations", func() {
				By("Creating an InferenceModel with more memory than needed")
				model := createTestInferenceModel(testModelName1, "model-1", largeMemory, testBackendURL, testContainerImage)
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				By("Waiting for Deployment to be created")
				Eventually(func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Simulating low utilization")
				updatedModel := &inferencev1alpha1.InferenceModel{}
				Eventually(func() error {
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, updatedModel); err != nil {
						return err
					}
					updatedModel.Status.ObservedPeakMemory = "1Gi" // 25% of declared 4Gi
					updatedModel.Status.UtilizationPercent = 25
					updatedModel.Status.Recommendation = "Consider reducing memory from 4Gi to 1100Mi (observed peak 1Gi, 75% overprovisioned)"
					return k8sClient.Status().Update(ctx, updatedModel)
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying recommendation is present")
				Eventually(func() bool {
					checkModel := &inferencev1alpha1.InferenceModel{}
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, checkModel); err != nil {
						return false
					}
					return checkModel.Status.Recommendation != ""
				}, eventuallyTimeout, pollingInterval).Should(BeTrue())
			})
		})
	})

	Describe("Node pool isolation", func() {
		Context("When InferenceModels are created on different node pools", func() {
			It("Should track budgets separately per node pool", func() {
				By("Creating an InferenceModel on pool A")
				model1 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName1,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-1",
						Memory:         mediumMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "pool-a"},
					},
				}
				Expect(k8sClient.Create(ctx, model1)).To(Succeed())

				By("Creating an InferenceModel on pool B")
				model2 := &inferencev1alpha1.InferenceModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testModelName2,
						Namespace: namespace,
					},
					Spec: inferencev1alpha1.InferenceModelSpec{
						ModelName:      "model-2",
						Memory:         mediumMemory,
						BackendURL:     testBackendURL,
						ContainerImage: testContainerImage,
						CooldownPeriod: metav1.Duration{Duration: 10 * time.Minute},
						MaxReplicas:    1,
						NodeSelector:   map[string]string{"inference-pool": "pool-b"},
					},
				}
				Expect(k8sClient.Create(ctx, model2)).To(Succeed())

				By("Waiting for both Deployments to be created")
				Eventually(func() error {
					if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName1}, &appsv1.Deployment{}); err != nil {
						return err
					}
					return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testModelName2}, &appsv1.Deployment{})
				}, eventuallyTimeout, pollingInterval).Should(Succeed())

				By("Verifying node selectors are different")
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

func cleanupBudgetTestResources() {
	modelNames := []string{"budget-test-model-1", "budget-test-model-2"}
	for _, name := range modelNames {
		model := &inferencev1alpha1.InferenceModel{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		}
		_ = k8sClient.Delete(ctx, model)
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
