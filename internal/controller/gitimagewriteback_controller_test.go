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

	imagesv1alpha1 "github.com/improving/image-updater-operator/api/v1alpha1"
)

var _ = Describe("GitImageWriteback Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-writeback"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}

		AfterEach(func() {
			resource := &imagesv1alpha1.GitImageWriteback{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should reconcile a suspended resource without touching git", func() {
			resource := &imagesv1alpha1.GitImageWriteback{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
				Spec: imagesv1alpha1.GitImageWritebackSpec{
					URL:     "https://example.com/org/repo.git",
					Branch:  "main",
					Suspend: true,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &GitImageWritebackReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should reject a resource without a required url", func() {
			bad := &imagesv1alpha1.GitImageWriteback{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-writeback", Namespace: "default"},
				Spec:       imagesv1alpha1.GitImageWritebackSpec{Branch: "main"},
			}
			Expect(k8sClient.Create(ctx, bad)).NotTo(Succeed())
		})
	})
})
