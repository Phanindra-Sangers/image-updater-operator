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
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
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

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
	"github.com/saphire/image-updater-operator/internal/gitwriteback"
	"github.com/saphire/image-updater-operator/internal/workload"
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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// pending is a container whose bound policy selected an image the container does
// not yet carry and whose update has cleared the mode gate.
type pending struct {
	name      string
	container *corev1.Container
	ip        *imagesv1alpha1.ImagePolicy
	desired   string
}

// Reconcile applies each annotated container's selected image, either by
// patching the live workload (write-back: live, the default) or by committing
// the change to Git for a GitOps controller to sync (write-back: git).
func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	method := workload.WriteBack(annotations)

	var pendings []pending
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

		if !r.gate(obj, containerName, &ip, desired, effectiveMode(&ip, annotations), method) {
			continue
		}
		ipCopy := ip
		pendings = append(pendings, pending{name: containerName, container: container, ip: &ipCopy, desired: desired})
	}

	if len(pendings) == 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if method == workload.MethodGit {
		return ctrl.Result{RequeueAfter: requeueAfter}, r.writeBackGit(ctx, obj, annotations, pendings)
	}
	return r.patchLive(ctx, obj, pendings, requeueAfter)
}

// gate decides whether a pending update may proceed under the effective mode and
// write-back method, emitting the appropriate event when it may not.
func (r *WorkloadReconciler) gate(
	obj client.Object,
	containerName string,
	ip *imagesv1alpha1.ImagePolicy,
	desired string,
	mode imagesv1alpha1.UpdateMode,
	method workload.Method,
) bool {
	if method == workload.MethodLive && !r.Adapter.Mutable {
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
		return true

	default: // Automatic, or approved Approval
		return true
	}
}

// patchLive mutates the container images on the live object and patches it.
func (r *WorkloadReconciler) patchLive(
	ctx context.Context, obj client.Object, pendings []pending, requeueAfter time.Duration,
) (ctrl.Result, error) {
	base := obj.DeepCopyObject().(client.Object)
	for _, p := range pendings {
		old := p.container.Image
		p.container.Image = p.desired
		setLastUpdated(obj, p.name, p.desired)
		r.event(obj, corev1.EventTypeNormal, "ImageUpdated",
			fmt.Sprintf("container %q updated %s -> %s", p.name, old, p.desired))
	}
	if err := r.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("patched workload images", "name", obj.GetName())
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// writeBackGit clones the repo named in the workload annotations, applies each
// pending update to the configured target file, and commits and pushes once.
// It never patches the live object. Repeated runs are no-ops once Git already
// carries the selected tags.
func (r *WorkloadReconciler) writeBackGit(
	ctx context.Context, obj client.Object, annotations map[string]string, pendings []pending,
) error {
	cfg := workload.GitSettings(annotations)
	if cfg.Repo == "" || cfg.Target == "" {
		r.warn(obj, "WriteBackMisconfigured",
			"write-back: git requires the git-repo and write-back-target annotations")
		return nil
	}
	target, err := gitwriteback.ParseTarget(cfg.Target)
	if err != nil {
		r.warn(obj, "WriteBackMisconfigured", err.Error())
		return nil
	}

	auth, err := r.gitAuth(ctx, obj.GetNamespace(), cfg.Repo, cfg.Secret)
	if err != nil {
		r.warn(obj, "AuthError", err.Error())
		return nil
	}

	dir, err := os.MkdirTemp("", "writeback-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	repo, err := gitwriteback.Clone(ctx, cfg.Repo, cfg.Branch, dir, auth)
	if err != nil {
		r.warn(obj, "CloneError", err.Error())
		return nil
	}

	changed := map[string]struct{}{}
	var summaries []string
	for _, p := range pendings {
		nameKey, tagKey := workload.HelmKeys(annotations, p.name)
		rel, did, err := gitwriteback.Apply(dir, gitwriteback.Edit{
			Target:      target,
			Repository:  p.ip.Spec.ImageRepository,
			Tag:         p.ip.Status.LatestTag,
			Container:   p.name,
			HelmNameKey: nameKey,
			HelmTagKey:  tagKey,
		})
		if err != nil {
			r.warn(obj, "WriteBackError", fmt.Sprintf("container %q: %v", p.name, err))
			continue
		}
		if did {
			changed[rel] = struct{}{}
			summaries = append(summaries, fmt.Sprintf("%s: %s -> %s", rel, p.name, p.desired))
		}
	}

	if len(changed) == 0 {
		return nil
	}

	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	message := "chore(images): automated image update\n\n" + strings.Join(summaries, "\n")
	sha, err := gitwriteback.CommitAndPush(ctx, repo, paths,
		gitwriteback.Author{Name: "image-updater-operator", Email: "image-updater@saphire.com"},
		message, cfg.Branch, auth, true)
	if err != nil {
		r.warn(obj, "PushError", err.Error())
		return nil
	}
	r.event(obj, corev1.EventTypeNormal, "ImageCommitted",
		fmt.Sprintf("committed %s to %s@%s: %s", strings.Join(paths, ", "), cfg.Repo, cfg.Branch, sha[:min(7, len(sha))]))
	logf.FromContext(ctx).Info("committed workload images", "name", obj.GetName(), "sha", sha)
	return nil
}

// gitAuth loads Git credentials from the named Secret in the workload namespace.
// An empty secret name yields nil auth, for public HTTPS repositories.
func (r *WorkloadReconciler) gitAuth(ctx context.Context, namespace, url, secretName string) (transport.AuthMethod, error) {
	if secretName == "" {
		return nil, nil
	}
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &s); err != nil {
		return nil, fmt.Errorf("reading git secret %q: %w", secretName, err)
	}
	return gitwriteback.AuthFromSecret(url, s.Data)
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
