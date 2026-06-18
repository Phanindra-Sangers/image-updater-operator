#!/usr/bin/env bash
# End-to-end local test for image-updater-operator.
#
# Stands up a dedicated kind cluster with an in-cluster registry, pushes a few
# tagged images, runs the operator against the cluster, and verifies that an
# annotated Deployment is bumped to the tag selected by an ImagePolicy. Also
# exercises the webhook trigger and Approval mode.
#
# Requires: docker, kind, kubectl, go. Nothing external; fully reproducible.
#
# Usage:
#   hack/e2e-local.sh           # set up, test, and leave the cluster running
#   hack/e2e-local.sh --cleanup # tear down the cluster and registry afterwards
set -euo pipefail

CLUSTER="image-updater-test"
CTX="kind-${CLUSTER}"
REG_NAME="kind-registry"
REG_PORT="5001"
REG="localhost:${REG_PORT}"
REPO="${REG}/test-app"
OPERATOR_LOG="/tmp/image-updater-operator.log"
OPERATOR_PID=""
CLEANUP="${1:-}"

step() { printf '\n\033[1;34m=== %s ===\033[0m\n' "$*"; }

cleanup() {
  step "cleanup"
  [ -n "${OPERATOR_PID}" ] && kill "${OPERATOR_PID}" 2>/dev/null || true
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

step "install CRDs"
kubectl config use-context "${CTX}" >/dev/null
kubectl wait --for=condition=Ready node --all --timeout=90s
make install

step "run operator (background)"
go build -o bin/manager ./cmd/main.go
bin/manager \
  --metrics-bind-address=0 \
  --health-probe-bind-address=:8099 \
  --webhook-receiver-bind-address=:9099 >"${OPERATOR_LOG}" 2>&1 &
OPERATOR_PID=$!
for i in $(seq 1 120); do
  grep -q "Starting workers" "${OPERATOR_LOG}" && break
  [ "$i" -eq 120 ] && { echo "operator did not start"; tail -20 "${OPERATOR_LOG}"; exit 1; }
  sleep 1
done
echo "operator ready"

step "apply ImagePolicy + Deployment (Automatic, semver >=1.0.0 <2.0.0)"
kubectl apply -f - <<EOF
apiVersion: images.saphire.com/v1alpha1
kind: ImagePolicy
metadata: { name: test-app-stable, namespace: default }
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
    image-updater.saphire.com/policy.app: test-app-stable
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

assert_image() { # name expected
  for i in $(seq 1 60); do
    img=$(kubectl get deploy "$1" -o jsonpath='{.spec.template.spec.containers[0].image}')
    [ "$img" = "$2" ] && { echo "PASS: $1 image == $2 (after ~${i}s)"; return 0; }
    sleep 1
  done
  echo "FAIL: $1 image == $img, expected $2"; exit 1
}

step "assert Automatic update to 1.2.0 (2.0.0 excluded by range)"
assert_image web "${REPO}:1.2.0"

step "push 1.3.0 and trigger via webhook"
docker tag busybox:latest "${REPO}:1.3.0" && docker push -q "${REPO}:1.3.0" >/dev/null
curl -s -X POST "http://localhost:9099/webhook/generic" -d "{\"repository\":\"${REPO}\"}"; echo
assert_image web "${REPO}:1.3.0"

step "Approval mode: held until approved"
kubectl apply -f - <<EOF
apiVersion: images.saphire.com/v1alpha1
kind: ImagePolicy
metadata: { name: test-app-approval, namespace: default }
spec:
  imageRepository: ${REPO}
  interval: 1m
  updateMode: Approval
  registryRef: { insecure: true }
  policy:
    semver: { range: ">=1.0.0 <2.0.0" }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web-approval
  namespace: default
  annotations:
    image-updater.saphire.com/policy.app: test-app-approval
spec:
  replicas: 1
  selector: { matchLabels: { app: web-approval } }
  template:
    metadata: { labels: { app: web-approval } }
    spec:
      containers:
        - name: app
          image: ${REPO}:1.0.0
          command: ["sleep", "3600"]
EOF
sleep 10
img=$(kubectl get deploy web-approval -o jsonpath='{.spec.template.spec.containers[0].image}')
[ "$img" = "${REPO}:1.0.0" ] && echo "PASS: held at 1.0.0 pending approval" || { echo "FAIL: changed without approval ($img)"; exit 1; }
kubectl annotate deploy web-approval image-updater.saphire.com/approve.app=1.3.0 --overwrite >/dev/null
assert_image web-approval "${REPO}:1.3.0"

step "ALL CHECKS PASSED"
