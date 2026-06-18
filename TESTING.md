# Testing image-updater-operator

This covers a fully reproducible local test, plus how to point the operator at
ECR, GHCR, Nexus, JFrog Artifactory, and Docker Hub.

## Unit and envtest

```sh
make test
```

This runs the policy selection unit tests and the envtest controller suite (it
downloads the envtest control-plane binaries on first run). The controller test
verifies semver selection writes to status and that the CRD rejects a policy
without `imageRepository`.

## Local end-to-end (kind + in-cluster registry)

No external credentials needed. The script stands up a dedicated kind cluster
with a local registry, pushes tagged images, runs the operator, and asserts the
Automatic, webhook-trigger, and Approval flows.

```sh
hack/e2e-local.sh            # set up, test, leave the cluster running
hack/e2e-local.sh --cleanup  # same, then delete the cluster and registry
```

Expected output ends with `ALL CHECKS PASSED`. What it verifies:

- A Deployment annotated `image-updater.saphire.com/policy.app: test-app-stable`
  is bumped from `1.0.0` to `1.2.0` by a semver policy `>=1.0.0 <2.0.0`
  (`2.0.0` is correctly excluded).
- Pushing `1.3.0` and calling `POST /webhook/generic` triggers an immediate
  re-scan and update, without waiting for the poll interval.
- An Approval-mode policy holds the workload at `1.0.0` and emits
  `ApprovalRequired` until the `approve.app` annotation names the candidate tag.

### Doing it by hand

```sh
# Cluster context
kubectl config use-context kind-image-updater-test

# Install CRDs and run the operator locally against the cluster
make install
make run   # or: go run ./cmd/main.go --metrics-bind-address=0

# Apply a policy + workload
kubectl apply -f config/samples/images_v1alpha1_imagepolicy.yaml

# Observe
kubectl get imagepolicy -w
kubectl describe deployment web
kubectl get events --field-selector involvedObject.name=web
```

## Running against a real registry

Set `spec.imageRepository` to the full repository (no tag) and provide
credentials. Two credential paths are supported:

1. A `kubernetes.io/dockerconfigjson` Secret referenced by
   `spec.registryRef.secretName` (the same shape as an imagePullSecret).
2. The ambient default keychain (host docker config / cloud credential helpers /
   workload identity) when no `secretName` is set. This is how ECR/GCR/ACR
   short-lived credentials are picked up.

In all cases the policy is identical apart from `imageRepository` and creds:

```yaml
apiVersion: images.saphire.com/v1alpha1
kind: ImagePolicy
metadata: { name: app-stable }
spec:
  imageRepository: <REPO>          # see per-registry rows below
  interval: 5m
  updateMode: Automatic
  registryRef:
    secretName: regcred            # omit to use ambient credentials
  policy:
    semver: { range: ">=1.0.0 <2.0.0" }
```

### Docker Hub

`imageRepository: docker.io/<org>/<repo>` (public repos need no secret).

```sh
kubectl create secret docker-registry regcred \
  --docker-server=https://index.docker.io/v1/ \
  --docker-username=<user> --docker-password=<token>
```

### GHCR (GitHub Container Registry)

`imageRepository: ghcr.io/<org>/<repo>`. Use a PAT with `read:packages`.

```sh
kubectl create secret docker-registry regcred \
  --docker-server=ghcr.io \
  --docker-username=<github-user> --docker-password=<PAT>
```

### Nexus (Docker hosted repository)

`imageRepository: <nexus-host>:<port>/<repo>`, e.g. `nexus.example.com:8082/app`.

```sh
kubectl create secret docker-registry regcred \
  --docker-server=nexus.example.com:8082 \
  --docker-username=<user> --docker-password=<pass>
```

If Nexus is served over plain HTTP, set `spec.registryRef.insecure: true`.

### JFrog Artifactory (Docker repository)

`imageRepository: <subdomain>.jfrog.io/<docker-repo>/<image>`, or the
subdomain-less path form `<artifactory-host>/<docker-repo>/<image>`.

```sh
kubectl create secret docker-registry regcred \
  --docker-server=<subdomain>.jfrog.io \
  --docker-username=<user> --docker-password=<identity-token>
```

### Amazon ECR

`imageRepository: <account>.dkr.ecr.<region>.amazonaws.com/<repo>`.

ECR tokens are short-lived (12h), so a static Secret is not ideal. Preferred
options:

- On EKS, give the operator's ServiceAccount an IAM role (IRSA) with
  `ecr:GetAuthorizationToken`, `ecr:BatchGetImage`, and
  `ecr:DescribeImages`/`ListImages`. Leave `registryRef` unset; the ambient
  keychain resolves credentials from the pod's web identity.
- For a one-off test, mint a token and store it as a Secret (refresh as needed):

  ```sh
  TOKEN=$(aws ecr get-login-password --region <region>)
  kubectl create secret docker-registry regcred \
    --docker-server=<account>.dkr.ecr.<region>.amazonaws.com \
    --docker-username=AWS --docker-password="$TOKEN"
  ```

### GCR / Artifact Registry and ACR

Leave `registryRef` unset and rely on workload identity (GKE) or managed
identity (AKS); the ambient keychain handles `*.pkg.dev`, `gcr.io`, and
`*.azurecr.io`.

## Webhooks for immediate updates

The receiver listens on `--webhook-receiver-bind-address` (default `:9090`).
Expose it via a Service/Ingress and point your registry at it:

- Docker Hub: `POST /webhook/dockerhub`
- Harbor: `POST /webhook/harbor`
- Generic: `POST /webhook/generic` with body
  `{"repository":"<host>/<repo>"}` or `{"image":"<host>/<repo>:<tag>"}`

Set `WEBHOOK_RECEIVER_TOKEN` to require `Authorization: Bearer <token>`.

```sh
curl -X POST http://<host>:9090/webhook/generic \
  -H "Authorization: Bearer $WEBHOOK_RECEIVER_TOKEN" \
  -d '{"repository":"ghcr.io/org/app"}'
```

## Git write-back

The YAML target editors (helm, kustomize, manifest) are covered by Go tests. The
full clone/edit/commit/push cycle is covered too, against a local bare repo over
file:// (skipped automatically when the git binary is absent):

```sh
go test ./internal/gitwriteback/...
```

For a full cluster run, `hack/e2e-git-writeback.sh` stands up kind plus a local
registry, seeds a bare Git repo with a Helm values file, and verifies the
operator commits the selected tag to Git while leaving the live Deployment
untouched:

```sh
hack/e2e-git-writeback.sh            # set up, test, leave the cluster running
hack/e2e-git-writeback.sh --cleanup  # tear everything down afterwards
```

To try it by hand against a real repository, annotate the workload to write to Git
instead of patching live. There are no marker comments. Example for a Helm
values file:

1. Create Git credentials in the workload's namespace:

   ```sh
   kubectl create secret generic git-https \
     --from-literal=username=git --from-literal=password=<PAT>
   ```

2. Annotate the workload and create a matching `ImagePolicy` (same
   `imageRepository` as the image in the values file):

   ```yaml
   metadata:
     annotations:
       image-updater.saphire.com/policy.app: app-stable
       image-updater.saphire.com/write-back: git
       image-updater.saphire.com/git-repo: https://github.com/org/app-config.git
       image-updater.saphire.com/git-secret: git-https
       image-updater.saphire.com/write-back-target: helm:env/prod/values.yaml
       image-updater.saphire.com/helm.image-name.app: image.repository
       image-updater.saphire.com/helm.image-tag.app: image.tag
   ```

3. Once the policy selects a tag, the operator commits and pushes the change;
   your GitOps controller then syncs it.

   ```sh
   kubectl describe deploy web | sed -n '/Events:/,$p'   # ImageCommitted event
   git -C <your-checkout> log --oneline -1               # see the operator's commit
   ```

The `write-back: git` and live-patch methods are mutually exclusive per workload;
setting `write-back: git` keeps the operator from patching the live object.

## Selection strategies to try

```yaml
# Latest stable semver in the 1.x line
policy: { semver: { range: ">=1.0.0 <2.0.0" } }

# Highest numeric build tag (latest-style)
policy: { numerical: { order: desc } }

# Date-stamped nightly builds, newest first
filterTags: { pattern: '^nightly-' }
policy:
  regex: { pattern: '^nightly-(\d{8})$', extract: "$1", numeric: true, order: desc }
```
