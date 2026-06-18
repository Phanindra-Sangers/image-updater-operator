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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
	"github.com/saphire/image-updater-operator/internal/policy"
	"github.com/saphire/image-updater-operator/internal/registry"
)

const (
	conditionReady = "Ready"

	defaultInterval = 5 * time.Minute
	// minInterval guards against hammering registries when a tiny interval is set.
	minInterval = 30 * time.Second
)

// TagLister lists the tags available for a repository. It is a seam for tests to
// avoid real registry access; production uses defaultTagLister.
type TagLister func(ctx context.Context, repository string, dockerConfig []byte, insecure bool) ([]string, error)

// ImagePolicyReconciler scans a registry repository on an interval and records
// the tag selected by the policy into status. Workload reconciliation is handled
// separately by the WorkloadReconciler.
type ImagePolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// ListTags lists registry tags. When nil it defaults to defaultTagLister.
	ListTags TagLister
}

// defaultTagLister builds a keychain from the (optional) dockerconfigjson bytes
// and lists tags via go-containerregistry.
func defaultTagLister(ctx context.Context, repository string, dockerConfig []byte, insecure bool) ([]string, error) {
	kc, err := registry.BuildKeychain(dockerConfig)
	if err != nil {
		return nil, err
	}
	return registry.ListTags(ctx, repository, kc, insecure)
}

// +kubebuilder:rbac:groups=images.saphire.com,resources=imagepolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=images.saphire.com,resources=imagepolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=images.saphire.com,resources=imagepolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile scans the registry and updates ImagePolicy status.
func (r *ImagePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ip imagesv1alpha1.ImagePolicy
	if err := r.Get(ctx, req.NamespacedName, &ip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval := effectiveInterval(ip.Spec.Interval.Duration)

	if ip.Spec.Suspend {
		log.V(1).Info("policy suspended, skipping scan")
		return ctrl.Result{}, nil
	}

	dockerConfig, err := r.loadDockerConfig(ctx, &ip)
	if err != nil {
		return r.fail(ctx, &ip, "AuthError", err)
	}

	insecure := ip.Spec.RegistryRef != nil && ip.Spec.RegistryRef.Insecure
	tags, err := r.ListTags(ctx, ip.Spec.ImageRepository, dockerConfig, insecure)
	if err != nil {
		return r.fail(ctx, &ip, "ScanError", err)
	}

	selected, err := policy.Select(tags, ip.Spec)
	if err != nil {
		return r.fail(ctx, &ip, "PolicyError", err)
	}
	if selected == "" {
		return r.fail(ctx, &ip, "NoMatch",
			noMatchError{repo: ip.Spec.ImageRepository, scanned: len(tags)})
	}

	newImage := registry.JoinImage(ip.Spec.ImageRepository, selected)
	changed := ip.Status.LatestImage != newImage
	now := metav1.Now()

	ip.Status.LatestTag = selected
	ip.Status.LatestImage = newImage
	ip.Status.LastScanTime = &now
	ip.Status.ScannedTags = len(tags)
	ip.Status.ObservedGeneration = ip.Generation
	setReady(&ip, metav1.ConditionTrue, "ScanSucceeded",
		"selected tag "+selected+" from "+intToStr(len(tags))+" tags")

	if err := r.Status().Update(ctx, &ip); err != nil {
		// The policy may have been deleted while the scan was in flight; that is
		// not an error, there is simply nothing left to update.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if changed && r.Recorder != nil {
		r.Recorder.Eventf(&ip, corev1.EventTypeNormal, "TagSelected",
			"selected %s for repository %s", selected, ip.Spec.ImageRepository)
	}

	log.V(1).Info("scan complete", "selected", selected, "tags", len(tags), "requeueAfter", interval)
	return ctrl.Result{RequeueAfter: interval}, nil
}

// loadDockerConfig fetches the dockerconfigjson bytes from the referenced Secret,
// or returns nil when no secret is referenced (ambient credentials are used).
func (r *ImagePolicyReconciler) loadDockerConfig(ctx context.Context, ip *imagesv1alpha1.ImagePolicy) ([]byte, error) {
	if ip.Spec.RegistryRef == nil || ip.Spec.RegistryRef.SecretName == "" {
		return nil, nil
	}
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: ip.Namespace, Name: ip.Spec.RegistryRef.SecretName}
	if err := r.Get(ctx, key, &secret); err != nil {
		return nil, err
	}
	if data, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		return data, nil
	}
	if data, ok := secret.Data[corev1.DockerConfigKey]; ok {
		return data, nil
	}
	return nil, &missingDockerConfigError{secret: secret.Name}
}

func (r *ImagePolicyReconciler) fail(ctx context.Context, ip *imagesv1alpha1.ImagePolicy, reason string, cause error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(cause, "scan failed", "reason", reason)

	setReady(ip, metav1.ConditionFalse, reason, cause.Error())
	ip.Status.ObservedGeneration = ip.Generation
	if err := r.Status().Update(ctx, ip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Recorder != nil {
		r.Recorder.Event(ip, corev1.EventTypeWarning, reason, cause.Error())
	}
	// Requeue on the normal interval; transient errors recover on the next scan.
	return ctrl.Result{RequeueAfter: effectiveInterval(ip.Spec.Interval.Duration)}, nil
}

func setReady(ip *imagesv1alpha1.ImagePolicy, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ip.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i := range ip.Status.Conditions {
		if ip.Status.Conditions[i].Type == conditionReady {
			if ip.Status.Conditions[i].Status == status {
				cond.LastTransitionTime = ip.Status.Conditions[i].LastTransitionTime
			}
			ip.Status.Conditions[i] = cond
			return
		}
	}
	ip.Status.Conditions = append(ip.Status.Conditions, cond)
}

func effectiveInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultInterval
	}
	if d < minInterval {
		return minInterval
	}
	return d
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImagePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.ListTags == nil {
		r.ListTags = defaultTagLister
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&imagesv1alpha1.ImagePolicy{}).
		Named("imagepolicy").
		Complete(r)
}
