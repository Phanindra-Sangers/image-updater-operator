# image-updater-operator

A Kubernetes operator that keeps workload container images up to date by watching
external registries (Docker Hub, Nexus, Harbor, GHCR, ECR, and any Docker
Registry v2 compatible endpoint). It can update images two ways: by patching the
live workload directly, or by committing the new image to a Git repository so a
GitOps controller such as Argo CD syncs it. Selection rules support semver
ranges, regex with capture-group ordering, and pure numerical/alphabetical
ordering.

It is built with Kubebuilder and controller-runtime.

## Contents

- [How it works](#how-it-works)
- [Write-back: live cluster or Git (Argo CD)](#write-back-live-cluster-or-git-argo-cd)
- [Install](#install)
- [Quickstart](#quickstart)
- [Usage](#usage)
- [ImagePolicy reference](#imagepolicy-reference)
- [GitImageWriteback reference](#gitimagewriteback-reference)
- [Annotation reference](#annotation-reference)
- [Selection rules](#selection-rules)
- [Update modes](#update-modes)
- [Detection: polling and webhooks](#detection-polling-and-webhooks)
- [Registry authentication](#registry-authentication)
- [Examples](#examples)
- [Observability and troubleshooting](#observability-and-troubleshooting)
- [Develop and run](#develop-and-run)
- [Testing](#testing)
- [Architecture](#architecture)

## How it works

Configuration is split across one CRD and a set of workload annotations.

An `ImagePolicy` defines a repository to scan and the rule used to pick the
winning tag. Its controller scans the registry on `spec.interval`, evaluates the
policy, and records the result in `status.latestImage` / `status.latestTag`.

A generic workload reconciler watches Deployments, StatefulSets, DaemonSets,
ReplicaSets, CronJobs, Pods, and Jobs. A workload opts in by annotating each
container with the policy it should track. When the referenced policy selects a
newer image, the container image is set to `<imageRepository>:<selectedTag>`.

```
                       scans on interval / webhook
   ImagePolicy  ─────────────────────────────────────►  registry (tags)
       │                                                      │
       │  status.latestImage = repo:selectedTag  ◄────────────┘
       ▼
   Deployment/STS/DS/RS/CronJob/Pod  ── annotation: policy.<container> ──► image patched
```

Scanning happens once per policy and fans out to every workload that references
it, so many workloads can share one policy without multiplying registry calls.
Jobs are reported but not patched, because their pod template is immutable after
creation.

## Write-back: live cluster or Git (Argo CD)

There are two ways to apply an update, and they are mutually exclusive per
workload. Choose based on whether the workload is managed by a GitOps tool.

**Live patch (not GitOps-managed).** Annotate the workload with
`policy.<container>`. The operator patches the live object. Use this when the
manifests are applied directly (`kubectl apply`, Helm install) and nothing else
owns the cluster state.

**Git write-back (Argo CD-managed).** Do not annotate the workload. Instead place
a marker comment next to the image value in your Git source and create a
`GitImageWriteback` pointing at the repo. The operator commits the new image to
Git, and Argo CD syncs it. The operator never touches the live object.

This split matters because Argo CD treats Git as the source of truth. If the
operator patched a live workload that Argo CD manages, Argo CD would mark the app
`OutOfSync` and, with self-heal enabled, revert the image. So for Argo CD
workloads you change Git and let Argo CD roll it out. Do not enable both for the
same workload.

Markers are line comments naming the policy, and they work in Helm values (map or
array form) and in plain manifests:

```yaml
# Helm values.yaml: separate repository and tag
image:
  repository: ghcr.io/org/app
  tag: 1.2.0  # {"$image-policy": "app-stable"}

# Array of images, full string form
containers:
  - image: ghcr.io/org/app:1.2.0  # {"$image-policy": "app-stable:image"}
```

The marker value is `<policyName>` or `<policyName>:<field>`, where field is
`tag` (default, writes the selected tag), `image` (writes the full
`repository:tag`), or `name` (writes the repository). Editing is line-based and
preserves formatting and quoting, so each commit changes exactly the marked line.

## Install

Requires a cluster and `kubectl`. Install the CRD and the controller:

```sh
# 1. Install the CRD
make install

# 2a. Run the controller locally against your current kube context (dev)
make run

# 2b. Or build an image and deploy it into the cluster (prod)
make docker-build docker-push IMG=<registry>/image-updater-operator:tag
make deploy IMG=<registry>/image-updater-operator:tag
```

`make deploy` installs the controller into the `image-updater-operator-system`
namespace with the RBAC it needs (read/patch on the supported workload kinds,
read on Secrets, and full access to `imagepolicies`).

## Quickstart

Track the latest 1.x of nginx and have a Deployment follow it automatically.

```sh
kubectl apply -f - <<'EOF'
apiVersion: images.improving.com/v1alpha1
kind: ImagePolicy
metadata:
  name: nginx-stable
spec:
  imageRepository: docker.io/library/nginx
  interval: 5m
  updateMode: Automatic
  policy:
    semver:
      range: ">=1.0.0 <2.0.0"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  annotations:
    image-updater.improving.com/policy.app: nginx-stable
spec:
  replicas: 1
  selector: { matchLabels: { app: web } }
  template:
    metadata: { labels: { app: web } }
    spec:
      containers:
        - name: app
          image: docker.io/library/nginx:1.0.0
EOF

# Watch the policy resolve a tag, then the deployment get patched
kubectl get imagepolicy nginx-stable -w
kubectl get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}'
```

The container named `app` is bound to the `nginx-stable` policy. Once the policy
scans and selects the highest 1.x tag, the operator patches
`web`'s `app` container to that image and records an `ImageUpdated` event.

## Usage

Every workflow starts with an `ImagePolicy` that defines what to scan and how to
pick the winning tag. From there you choose how the update is applied. The four
recipes below cover the common cases. Field-level detail is in the reference
sections that follow.

### Patch a live workload

For workloads applied directly (`kubectl apply`, Helm install) and not managed by
a GitOps tool. Create the policy, then bind each container to it with a
`policy.<container>` annotation on the workload. The operator patches the live
object in place. This is the flow shown in the [Quickstart](#quickstart).

```yaml
metadata:
  annotations:
    image-updater.improving.com/policy.app: nginx-stable
```

### Write the update to Git for Argo CD

For workloads whose source of truth is Git and that are synced by Argo CD. Do not
annotate the workload. Mark the image line in your Git source and create a
`GitImageWriteback` pointing at the repo. The operator commits the new image and
Argo CD rolls it out, so the live object is never patched directly.

```sh
kubectl create secret generic git-https \
  --from-literal=username=git \
  --from-literal=password=<personal-access-token>

kubectl apply -f - <<'EOF'
apiVersion: images.improving.com/v1alpha1
kind: GitImageWriteback
metadata:
  name: app-repo
spec:
  url: https://github.com/org/app-config.git
  branch: main
  path: .
  auth:
    httpsSecretRef:
      name: git-https
EOF
```

In the Git source, mark the line the operator should edit:

```yaml
image:
  repository: ghcr.io/org/app
  tag: 1.2.0  # {"$image-policy": "app-stable"}
```

Do not enable both the live-patch annotation and Git write-back for the same
workload. See [Write-back](#write-back-live-cluster-or-git-argo-cd) for why the
two are mutually exclusive.

### Require approval before updating

Set the policy to `Approval` mode, or override per workload with the
`update-mode` annotation. The operator records the candidate and emits an
`ApprovalRequired` event instead of patching. Approve the candidate to release it.

```sh
kubectl annotate deploy web \
  image-updater.improving.com/approve.app=1.4.0 --overwrite
```

### Report updates without applying them

Set `DryRun` mode to surface available updates as `UpdateAvailable` events while
leaving the workload untouched. Useful for sensitive workloads or for evaluating
a policy before trusting it.

```yaml
metadata:
  annotations:
    image-updater.improving.com/policy.app: app-stable
    image-updater.improving.com/update-mode: DryRun
```

## ImagePolicy reference

`apiVersion: images.improving.com/v1alpha1`, `kind: ImagePolicy` (namespaced).
A workload references a policy in its own namespace.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.imageRepository` | string | yes | | Repository to scan, no tag. e.g. `docker.io/library/nginx`, `ghcr.io/org/app`, `<acct>.dkr.ecr.<region>.amazonaws.com/app`. Bound containers are set to `<imageRepository>:<selectedTag>`. |
| `spec.policy` | object | yes | | The selection rule. Set exactly one of `semver`, `regex`, `numerical`, `alphabetical`. |
| `spec.filterTags.pattern` | string (regex) | no | | Pre-filter applied to the tag list before the policy runs. |
| `spec.interval` | duration | no | `5m` | Scan cadence. Clamped to a 30s minimum. |
| `spec.updateMode` | enum | no | `Automatic` | `Automatic`, `Approval`, or `DryRun`. Overridable per workload. |
| `spec.registryRef.secretName` | string | no | | `kubernetes.io/dockerconfigjson` Secret in the policy's namespace. Omit to use ambient credentials. |
| `spec.registryRef.insecure` | bool | no | `false` | Allow plain HTTP. Use only for trusted in-cluster registries. |
| `spec.suspend` | bool | no | `false` | Pause scanning and updates for this policy. |

Status (read-only):

| Field | Description |
|-------|-------------|
| `status.latestTag` | Tag selected at the last successful scan. |
| `status.latestImage` | Full `repository:tag` applied to bound containers. |
| `status.lastScanTime` | Timestamp of the last successful scan. |
| `status.scannedTags` | Number of tags the registry returned. |
| `status.conditions[Ready]` | `True` after a successful scan, `False` with a reason on error (`AuthError`, `ScanError`, `PolicyError`, `NoMatch`). |

## GitImageWriteback reference

`apiVersion: images.improving.com/v1alpha1`, `kind: GitImageWriteback`
(namespaced). It commits marked image updates to a Git repo and never patches
live workloads. See [config/samples](config/samples/images_v1alpha1_gitimagewriteback.yaml)
for a complete example.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.url` | string | yes | | Git remote. HTTPS (`https://host/org/repo.git`) or SSH (`git@host:org/repo.git`). |
| `spec.branch` | string | no | `main` | Branch to read and write. |
| `spec.path` | string | no | `.` | Subtree scanned for marker comments. |
| `spec.auth.httpsSecretRef.name` | string | no | | Secret with `username` and `password` (or `token`). |
| `spec.auth.sshSecretRef.name` | string | no | | Secret with `identity` (PEM key), optional `known_hosts`, optional `password` (passphrase). SSH is preferred if both are set. |
| `spec.commit.authorName` / `authorEmail` | string | no | operator defaults | Commit identity. |
| `spec.commit.messageTemplate` | string | no | `chore(images)...` | Commit message; `{{updates}}` expands to the list of edits. |
| `spec.commit.push` | bool | no | `true` | Push after committing. `false` commits locally only. |
| `spec.policies` | []string | no | | Allowlist of policy names; empty means any policy named by a marker. |
| `spec.interval` | duration | no | `5m` | How often the repo is checked. |
| `spec.suspend` | bool | no | `false` | Pause the writeback. |

Status exposes `lastCommitSHA`, `lastRunTime`, `updatedImages`, and a `Ready`
condition (reasons `Committed`, `UpToDate`, or an error such as `CloneError`,
`PushError`).

Credentials live in a Secret in the same namespace. HTTPS:

```sh
kubectl create secret generic git-https \
  --from-literal=username=git \
  --from-literal=password=<personal-access-token>
```

SSH:

```sh
kubectl create secret generic git-ssh \
  --from-file=identity=$HOME/.ssh/id_ed25519 \
  --from-file=known_hosts=$HOME/.ssh/known_hosts
```

## Annotation reference

All keys use the prefix `image-updater.improving.com/`. They go on the workload
object (Deployment, StatefulSet, and so on), not on the pod template.

| Annotation | Value | Purpose |
|------------|-------|---------|
| `policy.<container>` | ImagePolicy name | Bind a container to a policy. The suffix is the container name; works for regular, init, and sidecar containers. Repeat the key per container. |
| `update-mode` | `Automatic` \| `Approval` \| `DryRun` | Override the policy's update mode for this workload. |
| `approve.<container>` | a tag, e.g. `"1.4.0"` | In `Approval` mode, approve the named candidate tag for that container. |
| `last-updated.<container>` | set by the operator | Records the last image the operator wrote for that container (auditing). |

## Selection rules

Set exactly one of the following under `spec.policy`:

| Rule | Fields | Behavior |
|------|--------|----------|
| `semver` | `range` | Highest tag satisfying the constraint. A leading `v` is stripped before parsing, so `v1.10` is treated as `1.10.0`. |
| `regex` | `pattern`, `extract`, `numeric`, `order` | Keep tags matching `pattern`, optionally rewrite each via `extract` (`$1` capture syntax), sort lexically or numerically, take the `asc`/`desc` endpoint. |
| `numerical` | `order` | Natural-numeric ordering of all tags (so `9` sorts before `10`). |
| `alphabetical` | `order` | Lexical ordering of all tags. |

`order` defaults to `desc` (newest/highest first). `spec.filterTags.pattern`
optionally narrows candidates before the rule runs.

## Update modes

`spec.updateMode`, overridable per workload via the `update-mode` annotation:

- `Automatic` patches the workload as soon as a newer tag is selected.
- `Approval` records the candidate and emits an `ApprovalRequired` event; the
  workload is patched only once it carries
  `image-updater.improving.com/approve.<container>: "<tag>"` matching the candidate.
- `DryRun` reports the available update via an `UpdateAvailable` event but never
  patches.

## Detection: polling and webhooks

Polling is always on (`spec.interval`, 30s minimum). In addition, the operator
serves a webhook receiver (default `:9090`) that triggers an immediate re-scan of
the affected policies on a registry push event:

- `POST /webhook/dockerhub`
- `POST /webhook/harbor`
- `POST /webhook/generic` — body `{"repository":"host/repo"}` or `{"image":"host/repo:tag"}`

Set `WEBHOOK_RECEIVER_TOKEN` to require `Authorization: Bearer <token>`. Disable
the receiver with `--enable-webhook-receiver=false`. The receiver maps the pushed
repository to matching policies and forces their reconcile.

## Registry authentication

Reference a `kubernetes.io/dockerconfigjson` Secret (the same shape as an
`imagePullSecret`) via `spec.registryRef.secretName`. When omitted, the operator
falls back to the ambient default keychain, which is how ECR/GCR/ACR credential
helpers and cloud workload identity (for example IRSA on EKS) surface
credentials. Use `spec.registryRef.insecure: true` only for trusted in-cluster
registries served over plain HTTP.

Per-registry setup (Docker Hub, GHCR, Nexus, JFrog, ECR, GCR/ACR) is documented
in [TESTING.md](TESTING.md#running-against-a-real-registry).

## Examples

Init and sidecar containers, each on its own policy:

```yaml
metadata:
  annotations:
    image-updater.improving.com/policy.migrate: db-migrate-stable   # init container
    image-updater.improving.com/policy.app: app-stable              # main container
    image-updater.improving.com/policy.proxy: envoy-stable          # sidecar
```

Report-only (no patching) for a sensitive workload:

```yaml
metadata:
  annotations:
    image-updater.improving.com/policy.app: app-stable
    image-updater.improving.com/update-mode: DryRun
```

Track the highest numeric build tag (latest-style), ignoring non-numeric tags:

```yaml
spec:
  policy:
    numerical: { order: desc }
```

Date-stamped nightly builds, newest first:

```yaml
spec:
  filterTags: { pattern: '^nightly-' }
  policy:
    regex: { pattern: '^nightly-(\d{8})$', extract: "$1", numeric: true, order: desc }
```

Approve a staged update:

```sh
# Approval-mode policy raised an ApprovalRequired event with candidate 1.4.0
kubectl annotate deploy web \
  image-updater.improving.com/approve.app=1.4.0 --overwrite
```

## Observability and troubleshooting

```sh
# Is the policy resolving a tag?
kubectl get imagepolicy
kubectl get imagepolicy <name> -o jsonpath='{.status}' | jq

# Why did a workload not update? Read the events on the policy and the workload.
kubectl describe imagepolicy <name>
kubectl get events --field-selector involvedObject.name=<workload>
```

Common signals:

- `status.conditions[Ready] = False` with `AuthError` or `ScanError`: bad or
  missing credentials, wrong `imageRepository`, or the registry is unreachable.
- `NoMatch`: the registry returned tags but none satisfied the policy or
  `filterTags`. Check the `range`/`pattern`.
- Workload shows `ApprovalRequired` and does not change: the policy is in
  `Approval` mode; set the `approve.<container>` annotation.
- A `Job` shows `ImmutableWorkload`: Jobs cannot be patched after creation;
  recreate the Job to pick up the new image.

## Develop and run

Requires Go 1.24+, kubebuilder, and a cluster (kind works well).

```sh
make manifests generate   # regenerate CRDs and deepcopy after API changes
make test                 # unit tests + envtest
make install              # install CRDs into the current cluster
make run                  # run the manager locally against the current kube context
```

## Testing

See [TESTING.md](TESTING.md). For a fully reproducible end-to-end run against a
local registry in kind:

```sh
hack/e2e-local.sh            # set up, test, leave the cluster running
hack/e2e-local.sh --cleanup  # tear everything down afterwards
```

It verifies the Automatic, webhook-triggered, and Approval update flows, and the
guide documents pointing the operator at ECR, GHCR, Nexus, JFrog, and Docker Hub.

## Architecture

| Package | Responsibility |
|---------|----------------|
| `api/v1alpha1` | `ImagePolicy` and `GitImageWriteback` CRD types |
| `internal/policy` | Tag selection (semver/regex/numeric/alpha) |
| `internal/registry` | Tag listing via go-containerregistry, dockerconfigjson keychain |
| `internal/workload` | Annotation contract and per-kind pod-spec adapters |
| `internal/gitwriteback` | Marker parsing, surgical YAML line editing, and git clone/commit/push |
| `internal/controller` | `ImagePolicy` scan loop, the generic workload reconciler, and the `GitImageWriteback` reconciler |
| `internal/webhook` | Registry push-event receiver and the repository field index |
