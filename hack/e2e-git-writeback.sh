#!/usr/bin/env bash
# End-to-end local test for the Git write-back path.
#
# Stands up a dedicated kind cluster with an in-cluster registry, seeds a local
# bare Git repo with a Helm values.yaml, runs the operator against the cluster,
# and verifies that an annotated Deployment (write-back: git) causes the operator
# to commit the selected tag into the values file in Git, without ever patching
# the live workload.
#
# The repo is served over file:// so no Git server or network is needed. The
# operator runs on the host, so it can reach both the local registry and the
# local bare repo.
#
# Requires: docker, kind, kubectl, go, git. Fully reproducible.
#
# Usage:
#   hack/e2e-git-writeback.sh           # set up, test, and leave the cluster running
#   hack/e2e-git-writeback.sh --cleanup # tear down the cluster and registry afterwards
set -euo pipefail

CLUSTER="image-updater-gitwb"
CTX="kind-${CLUSTER}"
REG_NAME="kind-registry"
REG_PORT="5001"
REG="localhost:${REG_PORT}"
REPO="${REG}/test-app"
OPERATOR_LOG="/tmp/image-updater-gitwb.log"
OPERATOR_PID=""
WORKDIR="$(mktemp -d)"
BARE="${WORKDIR}/config.git"
CLEANUP="${1:-}"

step() { printf '\n\033[1;34m=== %s ===\033[0m\n' "$*"; }

cleanup() {
  step "cleanup"
  [ -n "${OPERATOR_PID}" ] && kill "${OPERATOR_PID}" 2>/dev/null || true
  rm -rf "${WORKDIR}"
  if [ "${CLEANUP}" = "--cleanup" ]; then
    kind delete cluster --name "${CLUSTER}" || true
    docker rm -f "${REG_NAME}" || true
  else
    echo "cluster '${CLUSTER}' and registry '${REG_NAME}' left running."
    echo "re-run with --cleanup to remove them."
  fi
}
trap cleanup EXIT

step "start local registry"
if [ "$(docker inspect -f '{{.State.Running}}' "${REG_NAME}" 2>/dev/null || true)" != 'true' ]; then
  docker run -d --restart=always -p "127.0.0.1:${REG_PORT}:5000" --name "${REG_NAME}" registry:2
fi

step "create kind cluster ${CLUSTER}"
if ! kind get clusters | grep -qx "${CLUSTER}"; then
  cat <<EOF | kind create cluster --name "${CLUSTER}" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
EOF
fi

step "wire registry into the cluster"
docker network connect "kind" "${REG_NAME}" 2>/dev/null || true
for node in $(kind get nodes --name "${CLUSTER}"); do
  docker exec "${node}" mkdir -p "/etc/containerd/certs.d/localhost:${REG_PORT}"
  echo "[host.\"http://${REG_NAME}:5000\"]" | \
    docker exec -i "${node}" cp /dev/stdin "/etc/containerd/certs.d/localhost:${REG_PORT}/hosts.toml"
done

step "push tagged images"
docker pull -q busybox:latest >/dev/null
for v in 1.0.0 1.1.0 1.2.0 2.0.0; do
  docker tag busybox:latest "${REPO}:${v}"
  docker push -q "${REPO}:${v}" >/dev/null
  echo "pushed ${REPO}:${v}"
done

step "seed bare Git repo with a Helm values.yaml"
SEED="${WORKDIR}/seed"
git init -q -b main "${SEED}"
cat >"${SEED}/values.yaml" <<EOF
image:
  repository: ${REPO}
  tag: 1.0.0
replicas: 1
EOF
git -C "${SEED}" -c user.email=seed@saphire.com -c user.name=seed add values.yaml
git -C "${SEED}" -c user.email=seed@saphire.com -c user.name=seed commit -q -m "seed values.yaml"
git init -q --bare -b main "${BARE}"
git -C "${SEED}" remote add origin "${BARE}"
git -C "${SEED}" push -q origin main
echo "seeded ${BARE} (values.yaml tag: 1.0.0)"

step "install CRDs"
kubectl config use-context "${CTX}" >/dev/null
kubectl wait --for=condition=Ready node --all --timeout=90s
make install

step "run operator (background)"
go build -o bin/manager ./cmd/main.go
bin/manager \
  --metrics-bind-address=0 \
  --health-probe-bind-address=:8098 \
  --webhook-receiver-bind-address=:9098 >"${OPERATOR_LOG}" 2>&1 &
OPERATOR_PID=$!
for i in $(seq 1 120); do
  grep -q "Starting workers" "${OPERATOR_LOG}" && break
  [ "$i" -eq 120 ] && { echo "operator did not start"; tail -20 "${OPERATOR_LOG}"; exit 1; }
  sleep 1
done
echo "operator ready"

step "apply ImagePolicy + Deployment (write-back: git, helm target)"
kubectl apply -f - <<EOF
apiVersion: images.saphire.com/v1alpha1
kind: ImagePolicy
metadata: { name: gitwb-stable, namespace: default }
spec:
  imageRepository: ${REPO}
  interval: 1m
  updateMode: Automatic
  registryRef: { insecure: true }
  policy:
    semver: { range: ">=1.0.0 <2.0.0" }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
  annotations:
    image-updater.saphire.com/policy.app: gitwb-stable
    image-updater.saphire.com/write-back: git
    image-updater.saphire.com/git-repo: file://${BARE}
    image-updater.saphire.com/git-branch: main
    image-updater.saphire.com/write-back-target: helm:values.yaml
    image-updater.saphire.com/helm.image-name.app: image.repository
    image-updater.saphire.com/helm.image-tag.app: image.tag
spec:
  replicas: 1
  selector: { matchLabels: { app: web } }
  template:
    metadata: { labels: { app: web } }
    spec:
      containers:
        - name: app
          image: ${REPO}:1.0.0
          command: ["sleep", "3600"]
EOF

step "assert the operator commits tag 1.2.0 to Git (2.0.0 excluded by range)"
for i in $(seq 1 90); do
  tag=$(git -C "${BARE}" show main:values.yaml 2>/dev/null | sed -n 's/^[[:space:]]*tag:[[:space:]]*//p')
  [ "${tag}" = "1.2.0" ] && { echo "PASS: values.yaml tag == 1.2.0 in Git (after ~${i}s)"; break; }
  [ "$i" -eq 90 ] && { echo "FAIL: values.yaml tag == ${tag:-<none>}, expected 1.2.0"; tail -30 "${OPERATOR_LOG}"; exit 1; }
  sleep 1
done

step "assert the live Deployment was NOT patched (git mode never touches it)"
img=$(kubectl get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}')
[ "${img}" = "${REPO}:1.0.0" ] && echo "PASS: live image still ${REPO}:1.0.0" \
  || { echo "FAIL: live image was patched to ${img}"; exit 1; }

step "assert idempotency: no second commit once Git is up to date"
sleep 20
commits=$(git -C "${BARE}" rev-list --count main)
[ "${commits}" = "2" ] && echo "PASS: exactly 2 commits (seed + one update)" \
  || { echo "FAIL: expected 2 commits, got ${commits}"; git -C "${BARE}" log --oneline; exit 1; }

step "the operator's commit"
git -C "${BARE}" log --oneline -1
echo
git -C "${BARE}" show main:values.yaml

step "ALL CHECKS PASSED"
