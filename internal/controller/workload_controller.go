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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagesv1alpha1 "github.com/improving/image-updater-operator/api/v1alpha1"
	"github.com/improving/image-updater-operator/internal/workload"
)

// policyRefIndex is the field-index key holding the ImagePolicy names referenced
// by a workload's annotations. It lets the ImagePolicy watch fan out to the
// workloads that depend on a policy.
const policyRefIndex = "imageupdater.policyRefs"

// requeueUnscanned is how long to wait before retrying a workload whose
// referenced policy has not produced a selected image yet.
const requeueUnscanned = 30 * time.Second

// WorkloadReconciler reconciles a single workload kind (described by Adapter)
// against the ImagePolicies its containers reference.
type WorkloadReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Adapter  workload.Adapter
}

// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets;replicasets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch

// Reconcile applies the selected image to each annotated container per its
// effective update mode.
func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("kind", r.Adapter.Name)

	obj := r.Adapter.New()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	annotations := obj.GetAnnotations()
	policies := workload.ContainerPolicies(annotations)
	if len(policies) == 0 {
		return ctrl.Result{}, nil
	}

	spec := r.Adapter.PodSpec(obj)
	if spec == nil {
		return ctrl.Result{}, nil
	}
	containers := indexContainers(spec)

	base := obj.DeepCopyObject().(client.Object)
	changed := false
	requeueAfter := time.Duration(0)

	for containerName, policyName := range policies {
		container, ok := containers[containerName]
		if !ok {
			r.warn(obj, "ContainerNotFound",
				fmt.Sprintf("annotation references container %q which does not exist", containerName))
			continue
		}

		var ip imagesv1alpha1.ImagePolicy
		key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: policyName}
		if err := r.Get(ctx, key, &ip); err != nil {
			r.warn(obj, "PolicyNotFound",
				fmt.Sprintf("container %q references ImagePolicy %q: %v", containerName, policyName, err))
			continue
		}

		if ip.Status.LatestImage == "" {
			// Policy has not completed its first scan; retry shortly.
			requeueAfter = requeueUnscanned
			continue
		}

		desired := ip.Status.LatestImage
		if container.Image == desired {
			continue
		}

		mode := effectiveMode(&ip, annotations)
		if !r.applyUpdate(obj, container, containerName, &ip, desired, mode) {
			continue
		}

		setLastUpdated(obj, containerName, desired)
		changed = true
	}

	if !changed {
		if requeueAfter > 0 {
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{}, nil
	}

	if err := r.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("updated workload images", "name", obj.GetName())
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// applyUpdate decides, based on the effective mode and workload mutability,
// whether to mutate the container image in place. It returns true when the
// container image was changed.
func (r *WorkloadReconciler) applyUpdate(
	obj client.Object,
	container *corev1.Container,
	containerName string,
	ip *imagesv1alpha1.ImagePolicy,
	desired string,
	mode imagesv1alpha1.UpdateMode,
) bool {
	if !r.Adapter.Mutable {
		r.warn(obj, "ImmutableWorkload",
			fmt.Sprintf("container %q could use %s but %s pod templates are immutable; recreate to apply",
				containerName, desired, r.Adapter.Name))
		return false
	}

	switch mode {
	case imagesv1alpha1.UpdateModeDryRun:
		r.event(obj, corev1.EventTypeNormal, "UpdateAvailable",
			fmt.Sprintf("container %q could update to %s (dry-run)", containerName, desired))
		return false

	case imagesv1alpha1.UpdateModeApproval:
		approved, ok := workload.ApprovedTag(obj.GetAnnotations(), containerName)
		if !ok || approved != ip.Status.LatestTag {
			r.event(obj, corev1.EventTypeNormal, "ApprovalRequired",
				fmt.Sprintf("container %q has candidate %s pending approval (set %s%s: %q)",
					containerName, desired, workload.ApproveContainerPrefix, containerName, ip.Status.LatestTag))
			return false
		}
		fallthrough

	default: // Automatic, or approved Approval
		old := container.Image
		container.Image = desired
		r.event(obj, corev1.EventTypeNormal, "ImageUpdated",
			fmt.Sprintf("container %q updated %s -> %s", containerName, old, desired))
		return true
	}
}

func effectiveMode(ip *imagesv1alpha1.ImagePolicy, annotations map[string]string) imagesv1alpha1.UpdateMode {
	if v, ok := annotations[workload.UpdateModeOverride]; ok {
		switch imagesv1alpha1.UpdateMode(v) {
		case imagesv1alpha1.UpdateModeAutomatic, imagesv1alpha1.UpdateModeApproval, imagesv1alpha1.UpdateModeDryRun:
			return imagesv1alpha1.UpdateMode(v)
		}
	}
	if ip.Spec.UpdateMode != "" {
		return ip.Spec.UpdateMode
	}
	return imagesv1alpha1.UpdateModeAutomatic
}

func indexContainers(spec *corev1.PodSpec) map[string]*corev1.Container {
	out := map[string]*corev1.Container{}
	for _, c := range workload.AllContainers(spec) {
		out[c.Name] = c
	}
	return out
}

func setLastUpdated(obj client.Object, container, image string) {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[workload.LastUpdatedContainerPrefix+container] = image
	obj.SetAnnotations(ann)
}

func (r *WorkloadReconciler) event(obj client.Object, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, msg)
	}
}

func (r *WorkloadReconciler) warn(obj client.Object, reason, msg string) {
	r.event(obj, corev1.EventTypeWarning, reason, msg)
}

// SetupWithManager registers the field index, the workload watch (filtered to
// annotated objects), and the ImagePolicy watch that fans out to dependents.
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), r.Adapter.New(), policyRefIndex,
		func(o client.Object) []string {
			policies := workload.ContainerPolicies(o.GetAnnotations())
			refs := make([]string, 0, len(policies))
			for _, name := range policies {
				refs = append(refs, name)
			}
			return refs
		}); err != nil {
		return err
	}

	annotated := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return len(workload.ContainerPolicies(o.GetAnnotations())) > 0
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(r.Adapter.New(), builder.WithPredicates(annotated)).
		Watches(&imagesv1alpha1.ImagePolicy{}, handler.EnqueueRequestsFromMapFunc(r.workloadsForPolicy)).
		Named("workload-" + r.Adapter.Name).
		Complete(r)
}

// workloadsForPolicy maps an ImagePolicy to the workloads of this reconciler's
// kind that reference it, using the field index.
func (r *WorkloadReconciler) workloadsForPolicy(ctx context.Context, ip client.Object) []reconcile.Request {
	list := r.Adapter.NewList()
	if err := r.List(ctx, list,
		client.InNamespace(ip.GetNamespace()),
		client.MatchingFields{policyRefIndex: ip.GetName()},
	); err != nil {
		logf.FromContext(ctx).Error(err, "listing workloads for policy", "policy", ip.GetName())
		return nil
	}

	var reqs []reconcile.Request
	for _, o := range listItems(list) {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: o.GetNamespace(), Name: o.GetName(),
		}})
	}
	return reqs
}
