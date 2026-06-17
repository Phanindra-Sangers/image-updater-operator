# image-updater-operator Helm chart

Deploys the image-updater-operator controller, its RBAC, ServiceAccount, and the
`ImagePolicy` CRD.

## Install

```sh
helm install image-updater ./helm-charts/image-updater-operator \
  --namespace image-updater-system --create-namespace \
  --set image.repository=<registry>/image-updater-operator \
  --set image.tag=0.1.0
```

If you loaded the image from the tar in `dist/` onto your nodes, set
`image.pullPolicy=IfNotPresent` (the default) so the kubelet uses the local copy.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Controller replicas. Use with `leaderElection.enabled`. |
| `image.repository` | `image-updater-operator` | Image repository. |
| `image.tag` | `""` | Image tag. Defaults to the chart `appVersion`. |
| `image.pullPolicy` | `IfNotPresent` | Pull policy. |
| `imagePullSecrets` | `[]` | Secrets for pulling the operator image. |
| `serviceAccount.create` | `true` | Create the ServiceAccount. |
| `rbac.create` | `true` | Create the ClusterRole and binding. |
| `leaderElection.enabled` | `true` | Pass `--leader-elect`. |
| `healthProbe.port` | `8081` | Health/readiness probe port. |
| `metrics.bindAddress` | `"0"` | Metrics bind address (`0` disables). |
| `dashboard.enabled` | `true` | Serve the read-only monitoring dashboard. |
| `dashboard.port` | `8082` | Dashboard port. |
| `dashboard.service.enabled` | `false` | Expose the dashboard with a Service (else port-forward). |
| `webhookReceiver.enabled` | `true` | Serve the registry push-event receiver. |
| `webhookReceiver.port` | `9090` | Receiver port. |
| `webhookReceiver.token` | `""` | Bearer token; stored in a chart-created Secret. |
| `webhookReceiver.existingSecret` | `""` | Use an existing Secret for the token instead. |
| `webhookReceiver.service.enabled` | `false` | Expose the receiver with a Service. |
| `resources` | requests 10m/64Mi, limits 500m/128Mi | Container resources. |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | Scheduling. |

## CRD

The `ImagePolicy` CRD ships in `crds/`. Helm installs CRDs on first install but
does not upgrade or delete them. On a chart upgrade that changes the CRD, apply
it yourself:

```sh
kubectl apply -f helm-charts/image-updater-operator/crds/
```
