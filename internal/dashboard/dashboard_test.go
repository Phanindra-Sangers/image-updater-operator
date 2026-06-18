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

package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
	"github.com/saphire/image-updater-operator/internal/workload"
)

func TestIndexEmbedded(t *testing.T) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatalf("index.html not embedded: %v", err)
	}
	if len(b) == 0 || !bytesContains(b, "image-updater-operator") {
		t.Fatalf("embedded index.html looks wrong (%d bytes)", len(b))
	}
}

func bytesContains(b []byte, sub string) bool {
	return len(b) > 0 && (len(sub) == 0 || indexOf(string(b), sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestOverview_EmptySerializesArrays(t *testing.T) {
	sc := scheme.Scheme
	if err := imagesv1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).Build()
	s := &Server{Client: cl}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))
	body := rec.Body.String()
	// The UI calls .reduce/.filter on these, so they must be [] not null.
	if !strings.Contains(body, `"policies":[]`) || !strings.Contains(body, `"workloads":[]`) {
		t.Fatalf("empty overview must serialize empty arrays, got: %s", body)
	}
}

func TestOverview(t *testing.T) {
	sc := scheme.Scheme
	if err := imagesv1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}

	policy := &imagesv1alpha1.ImagePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "app-stable", Namespace: "default"},
		Spec: imagesv1alpha1.ImagePolicySpec{
			ImageRepository: "ghcr.io/org/app",
			Policy:          imagesv1alpha1.Policy{Semver: &imagesv1alpha1.SemverPolicy{Range: ">=1.0.0"}},
		},
		Status: imagesv1alpha1.ImagePolicyStatus{
			LatestTag:   "1.4.0",
			LatestImage: "ghcr.io/org/app:1.4.0",
			Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Scanned"}},
		},
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "default",
			Annotations: map[string]string{
				workload.PolicyContainerPrefix + "app": "app-stable",
				workload.WriteBackMethod:               "git",
				workload.GitRepo:                       "https://github.com/org/cfg.git",
				workload.WriteBackTarget:               "helm:values.yaml",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "ghcr.io/org/app:1.0.0"}}},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(sc).
		WithObjects(policy, deploy).
		WithStatusSubresource(policy).Build()
	s := &Server{Client: cl}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var ov Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if len(ov.Policies) != 1 || ov.Policies[0].LatestTag != "1.4.0" {
		t.Fatalf("policy view wrong: %+v", ov.Policies)
	}
	if ov.Policies[0].Ready != "True" {
		t.Errorf("ready = %q, want True", ov.Policies[0].Ready)
	}
	if ov.Policies[0].WorkloadCount != 1 {
		t.Errorf("workloadCount = %d, want 1", ov.Policies[0].WorkloadCount)
	}
	if len(ov.Workloads) != 1 {
		t.Fatalf("want 1 workload, got %d", len(ov.Workloads))
	}
	w := ov.Workloads[0]
	if w.WriteBack != "git" || w.GitTarget != "helm:values.yaml" {
		t.Errorf("write-back fields wrong: %+v", w)
	}
	c := w.Containers[0]
	if c.Policy != "app-stable" || c.CurrentImage != "ghcr.io/org/app:1.0.0" || c.DesiredImage != "ghcr.io/org/app:1.4.0" {
		t.Errorf("container view wrong: %+v", c)
	}
	if c.UpToDate {
		t.Errorf("container should not be up to date (1.0.0 vs 1.4.0)")
	}
}
