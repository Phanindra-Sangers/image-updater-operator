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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// UpdateMode controls what the operator does when a newer image is selected.
// +kubebuilder:validation:Enum=Automatic;Approval;DryRun
type UpdateMode string

const (
	// UpdateModeAutomatic patches matching workloads as soon as a newer tag is selected.
	UpdateModeAutomatic UpdateMode = "Automatic"
	// UpdateModeApproval stages the selected tag in status and only patches workloads
	// once a human approves it via the approval annotation on the workload.
	UpdateModeApproval UpdateMode = "Approval"
	// UpdateModeDryRun only reports the selected tag via status and events; it never patches.
	UpdateModeDryRun UpdateMode = "DryRun"
)

// SortOrder controls the direction tags are ordered before picking a winner.
// +kubebuilder:validation:Enum=asc;desc
type SortOrder string

const (
	SortAscending  SortOrder = "asc"
	SortDescending SortOrder = "desc"
)

// SemverPolicy selects the highest tag that satisfies a semver range.
type SemverPolicy struct {
	// range is a semver constraint, e.g. ">=1.2.0 <2.0.0" or "~1.2.x".
	// See https://github.com/Masterminds/semver for the supported grammar.
	// +kubebuilder:validation:MinLength=1
	Range string `json:"range"`
}

// RegexPolicy filters tags by a regular expression and orders the matches.
type RegexPolicy struct {
	// pattern is an RE2 regular expression matched against each tag.
	// +kubebuilder:validation:MinLength=1
	Pattern string `json:"pattern"`

	// extract optionally rewrites each matching tag before ordering, using Go's
	// regexp expansion syntax (e.g. "$1" to keep the first capture group). When
	// empty, the full tag is used for ordering but the original tag is still applied.
	// +optional
	Extract string `json:"extract,omitempty"`

	// numeric, when true, orders the extracted values numerically rather than
	// lexicographically (so "10" sorts after "9").
	// +optional
	Numeric bool `json:"numeric,omitempty"`

	// order is the sort direction; the winning tag is the last one after sorting.
	// Defaults to desc (highest/newest first).
	// +kubebuilder:default=desc
	// +optional
	Order SortOrder `json:"order,omitempty"`
}

// OrderedPolicy selects a tag by pure numerical or alphabetical ordering.
type OrderedPolicy struct {
	// order is the sort direction. Defaults to desc.
	// +kubebuilder:default=desc
	// +optional
	Order SortOrder `json:"order,omitempty"`
}

// Policy is the tag selection rule. Exactly one of the fields must be set.
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type Policy struct {
	// +optional
	Semver *SemverPolicy `json:"semver,omitempty"`
	// +optional
	Regex *RegexPolicy `json:"regex,omitempty"`
	// +optional
	Numerical *OrderedPolicy `json:"numerical,omitempty"`
	// +optional
	Alphabetical *OrderedPolicy `json:"alphabetical,omitempty"`
}

// TagFilter optionally narrows the candidate tag set before the policy runs.
type TagFilter struct {
	// pattern is an RE2 regular expression; only matching tags are considered.
	// +kubebuilder:validation:MinLength=1
	Pattern string `json:"pattern"`
}

// RegistryRef points at credentials used to talk to the registry.
type RegistryRef struct {
	// secretName references a Secret of type kubernetes.io/dockerconfigjson in the
	// same namespace as the ImagePolicy (the same shape as an imagePullSecret).
	// When empty, the controller falls back to the cloud keychain (ECR/GCR/ACR
	// ambient credentials) and the host docker config.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// insecure allows talking to registries over plain HTTP. Use only for trusted
	// in-cluster registries.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
}

// ImagePolicySpec defines the desired state of ImagePolicy
type ImagePolicySpec struct {
	// imageRepository is the registry repository to scan, without a tag, e.g.
	// "docker.io/library/nginx", "ghcr.io/org/app", or "<account>.dkr.ecr.<region>.amazonaws.com/app".
	// A workload container that references this policy has its image set to
	// "<imageRepository>:<selectedTag>".
	// +kubebuilder:validation:MinLength=1
	// +required
	ImageRepository string `json:"imageRepository"`

	// policy is the tag selection rule.
	// +required
	Policy Policy `json:"policy"`

	// filterTags optionally pre-filters the tag list before the policy is applied.
	// +optional
	FilterTags *TagFilter `json:"filterTags,omitempty"`

	// registryRef configures how the controller authenticates to the registry.
	// +optional
	RegistryRef *RegistryRef `json:"registryRef,omitempty"`

	// interval is how often the registry is scanned. Defaults to 5m.
	// +kubebuilder:default="5m"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// updateMode controls what happens when a newer tag is selected. Defaults to Automatic.
	// A workload may override this via the update-mode annotation.
	// +kubebuilder:default=Automatic
	// +optional
	UpdateMode UpdateMode `json:"updateMode,omitempty"`

	// suspend pauses scanning and updates for this policy when true.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ImagePolicyStatus defines the observed state of ImagePolicy.
type ImagePolicyStatus struct {
	// latestTag is the tag selected by the policy at the last successful scan.
	// +optional
	LatestTag string `json:"latestTag,omitempty"`

	// latestImage is the fully-qualified image reference (repository:tag) for latestTag.
	// +optional
	LatestImage string `json:"latestImage,omitempty"`

	// lastScanTime is when the registry was last scanned successfully.
	// +optional
	LastScanTime *metav1.Time `json:"lastScanTime,omitempty"`

	// scannedTags is the number of tags returned by the registry at the last scan.
	// +optional
	ScannedTags int `json:"scannedTags,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the ImagePolicy resource.
	// Condition types: "Ready" (last scan succeeded and a tag is selected).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.imageRepository`
// +kubebuilder:printcolumn:name="LatestTag",type=string,JSONPath=`.status.latestTag`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.updateMode`
// +kubebuilder:printcolumn:name="LastScan",type=date,JSONPath=`.status.lastScanTime`

// ImagePolicy is the Schema for the imagepolicies API
type ImagePolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ImagePolicy
	// +required
	Spec ImagePolicySpec `json:"spec"`

	// status defines the observed state of ImagePolicy
	// +optional
	Status ImagePolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ImagePolicyList contains a list of ImagePolicy
type ImagePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ImagePolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImagePolicy{}, &ImagePolicyList{})
}
