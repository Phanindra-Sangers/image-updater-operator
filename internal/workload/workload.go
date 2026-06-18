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

// Package workload defines the annotation contract used to opt workloads into
// image updates and the per-kind adapters that expose each workload's pod spec
// in a uniform way so a single reconciler can drive every workload kind.
package workload

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AnnotationPrefix namespaces all annotations consumed by this operator.
	AnnotationPrefix = "image-updater.saphire.com/"

	// PolicyContainerPrefix maps a single container to an ImagePolicy by name.
	// Example: image-updater.saphire.com/policy.app: "frontend-stable".
	// The key suffix is the container name; init and sidecar containers are
	// addressed the same way since names are unique within a pod spec.
	PolicyContainerPrefix = AnnotationPrefix + "policy."

	// UpdateModeOverride overrides the ImagePolicy's updateMode for this workload.
	// Value is one of Automatic, Approval, DryRun.
	UpdateModeOverride = AnnotationPrefix + "update-mode"

	// ApproveContainerPrefix approves a specific tag for a container when the
	// effective update mode is Approval. The key suffix is the container name and
	// the value is the tag to approve, e.g.
	// image-updater.saphire.com/approve.app: "1.4.0".
	ApproveContainerPrefix = AnnotationPrefix + "approve."

	// LastUpdatedContainerPrefix records the last image this operator wrote for a
	// container. It is set by the controller and read for idempotency/auditing.
	// Only used for live patches; Git write-back relies on the repo for state.
	LastUpdatedContainerPrefix = AnnotationPrefix + "last-updated."

	// WriteBackMethod selects how the selected image is applied. "live" (default)
	// patches the running workload; "git" commits the change to a Git repository
	// and lets a GitOps controller sync it.
	WriteBackMethod = AnnotationPrefix + "write-back"

	// GitRepo is the clone URL written to when the method is "git". HTTPS
	// (https://host/org/repo.git) or SSH (git@host:org/repo.git).
	GitRepo = AnnotationPrefix + "git-repo"

	// GitBranch is the branch to read and write. Defaults to "main".
	GitBranch = AnnotationPrefix + "git-branch"

	// GitSecret names a Secret in the workload's namespace holding Git
	// credentials: keys "username"/"password" (or "token") for HTTPS, or
	// "identity" (+ optional "known_hosts", "password") for SSH.
	GitSecret = AnnotationPrefix + "git-secret"

	// WriteBackTarget points at the YAML to edit in the repo, as "kind:path":
	// "helm:env/prod/values.yaml", "kustomize:overlays/prod", or
	// "manifest:apps/web/deploy.yaml".
	WriteBackTarget = AnnotationPrefix + "write-back-target"

	// HelmImageNamePrefix gives the dotted values key holding the repository for
	// a container in a helm target, e.g. helm.image-name.app: "image.repository".
	HelmImageNamePrefix = AnnotationPrefix + "helm.image-name."

	// HelmImageTagPrefix gives the dotted values key holding the tag for a
	// container in a helm target, e.g. helm.image-tag.app: "image.tag".
	HelmImageTagPrefix = AnnotationPrefix + "helm.image-tag."
)

// Method is the write-back method selected for a workload.
type Method string

const (
	// MethodLive patches the running workload object in place.
	MethodLive Method = "live"
	// MethodGit commits the change to Git and never touches the live object.
	MethodGit Method = "git"
)

// WriteBack returns the effective write-back method for a workload, defaulting
// to live patching when the annotation is absent or unrecognized.
func WriteBack(annotations map[string]string) Method {
	if Method(annotations[WriteBackMethod]) == MethodGit {
		return MethodGit
	}
	return MethodLive
}

// GitConfig is the per-workload Git write-back configuration parsed from
// annotations. Branch defaults to "main" when unset.
type GitConfig struct {
	Repo   string
	Branch string
	Secret string
	Target string
}

// GitSettings extracts the Git write-back configuration from annotations.
func GitSettings(annotations map[string]string) GitConfig {
	branch := annotations[GitBranch]
	if branch == "" {
		branch = "main"
	}
	return GitConfig{
		Repo:   annotations[GitRepo],
		Branch: branch,
		Secret: annotations[GitSecret],
		Target: annotations[WriteBackTarget],
	}
}

// HelmKeys returns the dotted values keys for a container's repository and tag
// in a helm target. Either may be empty when not annotated.
func HelmKeys(annotations map[string]string, container string) (nameKey, tagKey string) {
	return annotations[HelmImageNamePrefix+container], annotations[HelmImageTagPrefix+container]
}

// ContainerPolicies returns a map of container name to ImagePolicy name parsed
// from the workload annotations.
func ContainerPolicies(annotations map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range annotations {
		if name, ok := strings.CutPrefix(k, PolicyContainerPrefix); ok && name != "" && v != "" {
			out[name] = v
		}
	}
	return out
}

// ApprovedTag returns the tag approved for a container, if any.
func ApprovedTag(annotations map[string]string, container string) (string, bool) {
	v, ok := annotations[ApproveContainerPrefix+container]
	return v, ok && v != ""
}

// Adapter exposes a workload kind's pod spec uniformly.
type Adapter struct {
	// Name is a short human-readable kind name used in logs and events.
	Name string
	// New returns a fresh, empty object of this kind.
	New func() client.Object
	// NewList returns a fresh, empty list object of this kind.
	NewList func() client.ObjectList
	// PodSpec returns a pointer to the pod spec carrying the containers to patch,
	// or nil when the object has no addressable pod template.
	PodSpec func(client.Object) *corev1.PodSpec
	// Mutable reports whether the image field can be patched after creation.
	// Jobs are immutable once created, so updates are reported but not applied.
	Mutable bool
}

// Adapters returns the adapter for every supported workload kind.
func Adapters() []Adapter {
	return []Adapter{
		{
			Name:    "Deployment",
			New:     func() client.Object { return &appsv1.Deployment{} },
			NewList: func() client.ObjectList { return &appsv1.DeploymentList{} },
			PodSpec: func(o client.Object) *corev1.PodSpec { return &o.(*appsv1.Deployment).Spec.Template.Spec },
			Mutable: true,
		},
		{
			Name:    "StatefulSet",
			New:     func() client.Object { return &appsv1.StatefulSet{} },
			NewList: func() client.ObjectList { return &appsv1.StatefulSetList{} },
			PodSpec: func(o client.Object) *corev1.PodSpec { return &o.(*appsv1.StatefulSet).Spec.Template.Spec },
			Mutable: true,
		},
		{
			Name:    "DaemonSet",
			New:     func() client.Object { return &appsv1.DaemonSet{} },
			NewList: func() client.ObjectList { return &appsv1.DaemonSetList{} },
			PodSpec: func(o client.Object) *corev1.PodSpec { return &o.(*appsv1.DaemonSet).Spec.Template.Spec },
			Mutable: true,
		},
		{
			Name:    "ReplicaSet",
			New:     func() client.Object { return &appsv1.ReplicaSet{} },
			NewList: func() client.ObjectList { return &appsv1.ReplicaSetList{} },
			PodSpec: func(o client.Object) *corev1.PodSpec { return &o.(*appsv1.ReplicaSet).Spec.Template.Spec },
			Mutable: true,
		},
		{
			Name:    "CronJob",
			New:     func() client.Object { return &batchv1.CronJob{} },
			NewList: func() client.ObjectList { return &batchv1.CronJobList{} },
			PodSpec: func(o client.Object) *corev1.PodSpec {
				return &o.(*batchv1.CronJob).Spec.JobTemplate.Spec.Template.Spec
			},
			Mutable: true,
		},
		{
			Name:    "Job",
			New:     func() client.Object { return &batchv1.Job{} },
			NewList: func() client.ObjectList { return &batchv1.JobList{} },
			PodSpec: func(o client.Object) *corev1.PodSpec { return &o.(*batchv1.Job).Spec.Template.Spec },
			Mutable: false,
		},
		{
			Name:    "Pod",
			New:     func() client.Object { return &corev1.Pod{} },
			NewList: func() client.ObjectList { return &corev1.PodList{} },
			PodSpec: func(o client.Object) *corev1.PodSpec { return &o.(*corev1.Pod).Spec },
			Mutable: true,
		},
	}
}

// AllContainers returns regular plus init containers from a pod spec, so callers
// can iterate every container with one loop.
func AllContainers(spec *corev1.PodSpec) []*corev1.Container {
	out := make([]*corev1.Container, 0, len(spec.Containers)+len(spec.InitContainers))
	for i := range spec.InitContainers {
		out = append(out, &spec.InitContainers[i])
	}
	for i := range spec.Containers {
		out = append(out, &spec.Containers[i])
	}
	return out
}
