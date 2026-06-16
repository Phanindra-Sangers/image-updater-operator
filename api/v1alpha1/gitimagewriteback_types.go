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

// GitAuth selects how the controller authenticates to the Git remote. Set one of
// the two secret references. If both are set, SSH is preferred.
type GitAuth struct {
	// httpsSecretRef references a Secret with keys "username" and "password"
	// (a personal access token works as the password). For GitHub fine-grained
	// or classic tokens, set username to your user or token name.
	// +optional
	HTTPSSecretRef *SecretRef `json:"httpsSecretRef,omitempty"`

	// sshSecretRef references a Secret with key "identity" (PEM private key),
	// optional "known_hosts", and optional "password" (key passphrase). Use this
	// with an SSH clone URL such as git@github.com:org/repo.git.
	// +optional
	SSHSecretRef *SecretRef `json:"sshSecretRef,omitempty"`
}

// SecretRef names a Secret in the same namespace as the GitImageWriteback.
type SecretRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// CommitSpec configures the commit the controller creates when files change.
type CommitSpec struct {
	// authorName is the Git author/committer name.
	// +kubebuilder:default="image-updater-operator"
	// +optional
	AuthorName string `json:"authorName,omitempty"`

	// authorEmail is the Git author/committer email.
	// +kubebuilder:default="image-updater@improving.com"
	// +optional
	AuthorEmail string `json:"authorEmail,omitempty"`

	// messageTemplate is the commit message. The token {{updates}} is replaced
	// with a newline-separated list of "file: policy -> value" entries.
	// +kubebuilder:default="chore(images): automated image update\n\n{{updates}}"
	// +optional
	MessageTemplate string `json:"messageTemplate,omitempty"`

	// push, when true (default), pushes the commit to the remote branch. When
	// false the commit is made locally only (useful for dry runs in tests).
	// +kubebuilder:default=true
	// +optional
	Push *bool `json:"push,omitempty"`
}

// GitImageWritebackSpec defines the desired state of GitImageWriteback
type GitImageWritebackSpec struct {
	// url is the Git remote, HTTPS (https://host/org/repo.git) or SSH
	// (git@host:org/repo.git). The auth method must match the URL scheme.
	// +kubebuilder:validation:MinLength=1
	// +required
	URL string `json:"url"`

	// branch is the branch to read and write. Defaults to "main".
	// +kubebuilder:default="main"
	// +optional
	Branch string `json:"branch,omitempty"`

	// path is the directory within the repository to scan for marker comments.
	// Defaults to "." (the whole repo).
	// +kubebuilder:default="."
	// +optional
	Path string `json:"path,omitempty"`

	// auth configures Git credentials. May be omitted for public repositories
	// over HTTPS.
	// +optional
	Auth *GitAuth `json:"auth,omitempty"`

	// commit configures the commit produced when image values change.
	// +optional
	Commit CommitSpec `json:"commit,omitempty"`

	// policies optionally restricts which ImagePolicy names this writeback will
	// apply. When empty, any policy named by a marker is honored.
	// +optional
	Policies []string `json:"policies,omitempty"`

	// interval is how often the repository is checked. Defaults to 5m.
	// +kubebuilder:default="5m"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// suspend pauses the writeback when true.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// GitImageWritebackStatus defines the observed state of GitImageWriteback.
type GitImageWritebackStatus struct {
	// lastCommitSHA is the SHA of the last commit this controller pushed.
	// +optional
	LastCommitSHA string `json:"lastCommitSHA,omitempty"`

	// lastRunTime is when the repository was last checked.
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// updatedImages lists the marker updates applied at the last successful run.
	// +optional
	UpdatedImages []string `json:"updatedImages,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the GitImageWriteback resource.
	// Condition type "Ready" is True after a successful run.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="LastCommit",type=string,JSONPath=`.status.lastCommitSHA`
// +kubebuilder:printcolumn:name="LastRun",type=date,JSONPath=`.status.lastRunTime`

// GitImageWriteback is the Schema for the gitimagewritebacks API
type GitImageWriteback struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of GitImageWriteback
	// +required
	Spec GitImageWritebackSpec `json:"spec"`

	// status defines the observed state of GitImageWriteback
	// +optional
	Status GitImageWritebackStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// GitImageWritebackList contains a list of GitImageWriteback
type GitImageWritebackList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []GitImageWriteback `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitImageWriteback{}, &GitImageWritebackList{})
}
