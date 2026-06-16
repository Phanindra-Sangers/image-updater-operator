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
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagesv1alpha1 "github.com/improving/image-updater-operator/api/v1alpha1"
	"github.com/improving/image-updater-operator/internal/gitwriteback"
	"github.com/improving/image-updater-operator/internal/registry"
)

// GitImageWritebackReconciler clones a Git repository, updates marked image
// values from ImagePolicy status, and commits/pushes the changes so a GitOps
// controller such as Argo CD can sync them. It never patches live workloads.
type GitImageWritebackReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=images.improving.com,resources=gitimagewritebacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=images.improving.com,resources=gitimagewritebacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=images.improving.com,resources=gitimagewritebacks/finalizers,verbs=update

// Reconcile performs one clone/update/commit cycle.
func (r *GitImageWritebackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var gw imagesv1alpha1.GitImageWriteback
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval := effectiveInterval(gw.Spec.Interval.Duration)
	if gw.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	auth, err := r.resolveAuth(ctx, &gw)
	if err != nil {
		return r.gwFail(ctx, &gw, "AuthError", err, interval)
	}

	dir, err := os.MkdirTemp("", "imgwriteback-")
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	repo, err := gitwriteback.Clone(ctx, gw.Spec.URL, branchOrDefault(gw.Spec.Branch), dir, auth)
	if err != nil {
		return r.gwFail(ctx, &gw, "CloneError", err, interval)
	}

	resolve := r.resolver(ctx, &gw)
	result, err := gitwriteback.ScanAndUpdate(dir, pathOrDefault(gw.Spec.Path), resolve)
	if err != nil {
		return r.gwFail(ctx, &gw, "UpdateError", err, interval)
	}

	now := metav1.Now()
	gw.Status.LastRunTime = &now
	gw.Status.ObservedGeneration = gw.Generation

	if len(result.Updates) == 0 {
		setGWReady(&gw, metav1.ConditionTrue, "UpToDate", "no marked images needed updating")
		gw.Status.UpdatedImages = nil
		if err := r.Status().Update(ctx, &gw); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	push := gw.Spec.Commit.Push == nil || *gw.Spec.Commit.Push
	sha, err := gitwriteback.CommitAndPush(ctx, repo, result.ChangedFiles,
		gitwriteback.Author{Name: authorName(gw.Spec.Commit), Email: authorEmail(gw.Spec.Commit)},
		commitMessage(gw.Spec.Commit, result.Updates),
		branchOrDefault(gw.Spec.Branch), auth, push)
	if err != nil {
		return r.gwFail(ctx, &gw, "PushError", err, interval)
	}

	gw.Status.LastCommitSHA = sha
	gw.Status.UpdatedImages = updateStrings(result.Updates)
	setGWReady(&gw, metav1.ConditionTrue, "Committed",
		"committed "+intToStr(len(result.Updates))+" image update(s) as "+shortSHA(sha))
	if err := r.Status().Update(ctx, &gw); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(&gw, corev1.EventTypeNormal, "Committed",
			"pushed %d image update(s) to %s (%s)", len(result.Updates), gw.Spec.URL, shortSHA(sha))
	}
	log.Info("git writeback committed", "sha", sha, "updates", len(result.Updates))
	return ctrl.Result{RequeueAfter: interval}, nil
}

// resolver returns a lookup from policy name to substitution values, honoring
// the optional policy allowlist.
func (r *GitImageWritebackReconciler) resolver(ctx context.Context, gw *imagesv1alpha1.GitImageWriteback) func(string) (gitwriteback.Resolved, bool) {
	allow := map[string]bool{}
	for _, p := range gw.Spec.Policies {
		allow[p] = true
	}
	return func(policy string) (gitwriteback.Resolved, bool) {
		if len(allow) > 0 && !allow[policy] {
			return gitwriteback.Resolved{}, false
		}
		var ip imagesv1alpha1.ImagePolicy
		if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: policy}, &ip); err != nil {
			return gitwriteback.Resolved{}, false
		}
		if ip.Status.LatestImage == "" || ip.Status.LatestTag == "" {
			return gitwriteback.Resolved{}, false
		}
		repo, _, err := registry.SplitImage(ip.Status.LatestImage)
		if err != nil {
			repo = ip.Spec.ImageRepository
		}
		return gitwriteback.Resolved{
			Tag:        ip.Status.LatestTag,
			Image:      ip.Status.LatestImage,
			Repository: repo,
		}, true
	}
}

func (r *GitImageWritebackReconciler) resolveAuth(ctx context.Context, gw *imagesv1alpha1.GitImageWriteback) (transport.AuthMethod, error) {
	if gw.Spec.Auth == nil {
		return nil, nil
	}
	// SSH is preferred when both are set.
	if ref := gw.Spec.Auth.SSHSecretRef; ref != nil {
		s, err := r.getSecret(ctx, gw.Namespace, ref.Name)
		if err != nil {
			return nil, err
		}
		return gitwriteback.SSHAuth(s.Data["identity"], string(s.Data["password"]), s.Data["known_hosts"])
	}
	if ref := gw.Spec.Auth.HTTPSSecretRef; ref != nil {
		s, err := r.getSecret(ctx, gw.Namespace, ref.Name)
		if err != nil {
			return nil, err
		}
		password := string(s.Data["password"])
		if password == "" {
			password = string(s.Data["token"])
		}
		return gitwriteback.HTTPSAuth(string(s.Data["username"]), password), nil
	}
	return nil, nil
}

func (r *GitImageWritebackReconciler) getSecret(ctx context.Context, ns, name string) (*corev1.Secret, error) {
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *GitImageWritebackReconciler) gwFail(ctx context.Context, gw *imagesv1alpha1.GitImageWriteback, reason string, cause error, interval time.Duration) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(cause, "git writeback failed", "reason", reason)
	setGWReady(gw, metav1.ConditionFalse, reason, cause.Error())
	gw.Status.ObservedGeneration = gw.Generation
	if err := r.Status().Update(ctx, gw); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(gw, corev1.EventTypeWarning, reason, cause.Error())
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitImageWritebackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&imagesv1alpha1.GitImageWriteback{}).
		Watches(&imagesv1alpha1.ImagePolicy{}, handler.EnqueueRequestsFromMapFunc(r.writebacksForPolicy)).
		Named("gitimagewriteback").
		Complete(r)
}

// writebacksForPolicy enqueues every GitImageWriteback in the policy's namespace
// when an ImagePolicy changes. Markers are only known after cloning, so this
// fans out coarsely; reconciles with no diff are cheap.
func (r *GitImageWritebackReconciler) writebacksForPolicy(ctx context.Context, ip client.Object) []reconcile.Request {
	var list imagesv1alpha1.GitImageWritebackList
	if err := r.List(ctx, &list, client.InNamespace(ip.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace, Name: list.Items[i].Name,
		}})
	}
	return reqs
}

func setGWReady(gw *imagesv1alpha1.GitImageWriteback, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: gw.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i := range gw.Status.Conditions {
		if gw.Status.Conditions[i].Type == conditionReady {
			if gw.Status.Conditions[i].Status == status {
				cond.LastTransitionTime = gw.Status.Conditions[i].LastTransitionTime
			}
			gw.Status.Conditions[i] = cond
			return
		}
	}
	gw.Status.Conditions = append(gw.Status.Conditions, cond)
}

func branchOrDefault(b string) string {
	if b == "" {
		return "main"
	}
	return b
}

func pathOrDefault(p string) string {
	if p == "" {
		return "."
	}
	return p
}

func authorName(c imagesv1alpha1.CommitSpec) string {
	if c.AuthorName == "" {
		return "image-updater-operator"
	}
	return c.AuthorName
}

func authorEmail(c imagesv1alpha1.CommitSpec) string {
	if c.AuthorEmail == "" {
		return "image-updater@improving.com"
	}
	return c.AuthorEmail
}

func commitMessage(c imagesv1alpha1.CommitSpec, updates []gitwriteback.Update) string {
	tmpl := c.MessageTemplate
	if tmpl == "" {
		tmpl = "chore(images): automated image update\n\n{{updates}}"
	}
	return strings.ReplaceAll(tmpl, "{{updates}}", strings.Join(updateStrings(updates), "\n"))
}

func updateStrings(updates []gitwriteback.Update) []string {
	out := make([]string, 0, len(updates))
	for _, u := range updates {
		out = append(out, u.String())
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
