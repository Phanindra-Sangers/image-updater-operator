/*
Copyright 2026.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
)

var _ = Describe("ImagePolicy Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}

		AfterEach(func() {
			resource := &imagesv1alpha1.ImagePolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should select the highest semver tag and record it in status", func() {
			resource := &imagesv1alpha1.ImagePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
				Spec: imagesv1alpha1.ImagePolicySpec{
					ImageRepository: "docker.io/library/nginx",
					Policy: imagesv1alpha1.Policy{
						Semver: &imagesv1alpha1.SemverPolicy{Range: ">=1.0.0 <2.0.0"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &ImagePolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				ListTags: func(_ context.Context, _ string, _ []byte, _ bool) ([]string, error) {
					return []string{"1.0.0", "1.2.5", "1.9.0", "2.0.0", "latest"}, nil
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &imagesv1alpha1.ImagePolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.LatestTag).To(Equal("1.9.0"))
			Expect(updated.Status.LatestImage).To(Equal("docker.io/library/nginx:1.9.0"))
			Expect(updated.Status.ScannedTags).To(Equal(5))
		})

		It("should reject a resource without a required imageRepository", func() {
			bad := &imagesv1alpha1.ImagePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-resource", Namespace: "default"},
				Spec: imagesv1alpha1.ImagePolicySpec{
					Policy: imagesv1alpha1.Policy{
						Semver: &imagesv1alpha1.SemverPolicy{Range: ">=1.0.0"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, bad)).NotTo(Succeed())
		})
	})
})
