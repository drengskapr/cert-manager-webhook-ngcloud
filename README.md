ACME webhook for ngcloud.ru
===

A [cert-manager](https://cert-manager.io) ACME DNS-01 webhook solver for the `ngcloud.ru` (Nubes) cloud platform. It automates TLS certificate issuance by creating and deleting DNS TXT records via the ngcloud.ru deck-api.

## How it works

### Overview

Cert-manager issues TLS certificates via the ACME protocol. For DNS-01 challenges, it needs to create a temporary TXT record at `_acme-challenge.<your-domain>` to prove domain ownership to the ACME server (e.g. Let's Encrypt). Once the certificate is issued, cert-manager removes the record.

The webhook acts as a bridge between cert-manager and the ngcloud.ru DNS API:

```
User creates a Certificate resource
        ↓
cert-manager creates a DNS-01 ChallengeRequest (TXT record needed)
        ↓
cert-manager HTTP POSTs to the webhook service (HTTPS, port 443)
        ↓
webhook.Present() fires:
  1. Reads zoneUID + API token from config / Kubernetes Secret
  2. Fetches CFS parameter IDs from Nubes deck-api
  3. Creates a DNS record instance (idempotent)
  4. Creates a "create" operation and pushes TXT record parameters
  5. Runs the operation and polls until completion
        ↓
cert-manager polls public DNS until the TXT record is visible
        ↓
ACME server validates → certificate issued
        ↓
cert-manager POSTs to the webhook again for cleanup
        ↓
webhook.CleanUp() fires: finds instance → runs "delete" operation → polls
```

### Running in a container

The webhook is compiled as a single static binary (no libc, no shell) and packaged in a `gcr.io/distroless/static` image. At startup the binary calls into the cert-manager webhook framework, which:

* Reads `--tls-cert-file` and `--tls-private-key-file` to serve HTTPS on port 6443
* Registers the solver under the configured `GROUP_NAME`
* Exposes `/healthz` for Kubernetes liveness and readiness probes
* Calls `Present()` / `CleanUp()` on the solver when cert-manager POSTs a challenge

`solver.go` is a plugin &mdash; the HTTP server, TLS, and Kubernetes registration are all handled by the framework.

### Network path

```
cert-manager pod
    → Kubernetes Service (ClusterIP, port 443 → 6443)
        → webhook pod (port 6443, HTTPS)
```

The `APIService` object registered by the Helm chart tells the Kubernetes API aggregation layer where the webhook lives. Cert-manager uses this to route `ChallengeRequest` POSTs.

### TLS PKI

The Helm chart (`pki.yaml`) provisions a two-tier certificate authority:

```
SelfSigned Issuer
    → CA Certificate (5 years, isCA: true)
        → CA Issuer
            → Serving Certificate (1 year)
```

The CA certificate's bundle is injected into the `APIService` object by cert-manager's cainjector so Kubernetes trusts the webhook's TLS. The serving cert rotates automatically every year with zero downtime &mdash; the CA lives long enough that the `APIService` bundle seldom needs manual updating.

### RBAC

The Helm chart creates two `ClusterRole` / `ClusterRoleBinding` pairs:

| Role            | Purpose                                                              |
|-----------------|----------------------------------------------------------------------|
| `secret-reader` | Allows the webhook pod to read Secrets (to fetch the API token)      |
| `domain-solver` | Allows the cert-manager ServiceAccount to call the webhook API group |


## Prerequisites

* Kubernetes cluster with cert-manager ≥ v1.19.4 installed
* cert-manager cainjector enabled (enabled by default)
* A Nubes account with DNS zone management access and an API token
* Managed zone UUID (for `Issuer`/`ClusterIssuer`)
* Docker (for building the image)
* Helm 3

## Building

### Binary

```bash
go build ./...
```

### Docker image

```bash
docker build -t cert-manager-webhook-ngcloud:latest .
```

Or using Make with a custom image name and tag:

```bash
make docker-build IMAGE=registry.example.com/cert-manager-webhook-ngcloud TAG=v1.0.0
```

Push to a registry accessible from your cluster:

```bash
docker push registry.example.com/cert-manager-webhook-ngcloud:v1.0.0
```

## Installation

### Deploy the webhook with Helm

```bash
helm install cert-manager-webhook-ngcloud deploy/cert-manager-webhook-ngcloud \
  --namespace cert-manager \
  --set image.repository=registry.example.com/cert-manager-webhook-ngcloud \
  --set image.tag=v1.0.0
```

To also create the API token Secret from Helm (optional, skip if you manage
the Secret yourself):

```bash
helm install cert-manager-webhook-ngcloud deploy/cert-manager-webhook-ngcloud \
  --namespace cert-manager \
  --set image.repository=registry.example.com/cert-manager-webhook-ngcloud \
  --set image.tag=v1.0.0 \
  --set apiToken=your-nubes-api-token
```

This creates a Secret named `ngcloud-api-token` in the `cert-manager` namespace.

If you prefer to manage the Secret yourself:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ngcloud-api-token
  namespace: cert-manager
type: Opaque
stringData:
  token: <your-nubes-api-token>
```

Verify the webhook pod is running:

```bash
kubectl get pods -n cert-manager -l app.kubernetes.io/name=cert-manager-webhook-ngcloud
```

### Set up cert-manager

Cert-manager requres setup to correctly resolve created TXT-records. If you have installed `cert-manager` using [official helm chart](https://cert-manager.io/docs/installation/helm/) add this to your values file:

```yaml
# -- explicitly set ip address of recursive dns server ns3.ngcloud.ru
dns01RecursiveNameservers: "185.247.187.83:53"
# -- enforce usage of recursive dns servers
# -- as cert-manager by default is using nonexistent ns.ngcloud.ru
# -- dns server
dns01RecursiveNameserversOnly: true
```

### Create an Issuer or ClusterIssuer

Zone name and zone UUID are configured in the cert-manager `Issuer`/`ClusterIssuer`, not in the Helm chart. cert-manager infers the zone name from `Certificate.spec.dnsNames` at runtime; you only need to supply the zone's UUID.

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-ngcloud
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    privateKeySecretRef:
      name: letsencrypt-account-key
    solvers:
      - dns01:
          webhook:
            groupName: acme.ngcloud.ru
            solverName: ngcloud
            config:
              zoneUID: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
              tokenSecretRef:
                name: ngcloud-api-token
                namespace: cert-manager
                key: token
```

> Find the zone UUID in the Nubes control panel, or via the deck-api.

### 3. Issue a certificate

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: example-tls
  namespace: default
spec:
  secretName: example-tls
  issuerRef:
    name: letsencrypt-ngcloud
    kind: ClusterIssuer
  dnsNames:
    - example.com
    - "*.example.com"
```

Monitor progress:

```bash
kubectl describe certificate example-tls -n default
kubectl get challenges -n default
```

## Testing

The conformance test spins up a real in-process Kubernetes API server (via `envtest`) and runs the full Present/CleanUp cycle against a live Nubes zone.

### Install envtest binaries

```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
~/go/bin/setup-envtest use --bin-dir /tmp/envtest-bins
```

### Prepare the API token secret

Fill in your real API token in `testdata/ngcloud/secret.yaml`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ngcloud-api-token
type: Opaque
stringData:
  token: your-nubes-api-token
```

> Do not commit a real token in this secret. Discarding changes is recommended upon test completion.

### Run the tests

```bash
ENVTEST_DIR=/tmp/envtest-bins/k8s/1.35.0-linux-amd64

TEST_ZONE_NAME=example.com \
TEST_ZONE_UID=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx \
TEST_ASSET_ETCD=$ENVTEST_DIR/etcd \
TEST_ASSET_KUBE_APISERVER=$ENVTEST_DIR/kube-apiserver \
TEST_ASSET_KUBECTL=$ENVTEST_DIR/kubectl \
go test ./... -v -timeout 10m
```

* `TEST_ZONE_NAME` &mdash; DNS zone to test against (must be managed in Nubes)
* `TEST_ZONE_UID` &mdash; UUID of that zone from the Nubes control panel

The test creates a real TXT record in your zone, verifies DNS propagation via
`ns3.ngcloud.ru` (185.247.187.83), then deletes the record.


## Helm chart configuration reference

### values.yaml

| Key                              | Default                        | Description |
|----------------------------------|--------------------------------|-------------|
| `image.repository`               | `cert-manager-webhook-ngcloud` | Container image repository |
| `image.tag`                      | `latest`                       | Container image tag |
| `image.pullPolicy`               | `IfNotPresent`                 | Image pull policy |
| `groupName`                      | `acme.ngcloud.ru`              | ACME webhook group name (must match Issuer config) |
| `apiToken`                       | `""`                           | Nubes API token; if set, creates the `ngcloud-api-token` Secret |
| `service.type`                   | `ClusterIP`                    | Webhook svc type: ClusterIP, NodePort, LoadBalancer |
| `service.port`                   | `443`                          | Webhook svc port |
| `containerPort`                  | `6443`                         | Port for webhook container to listen on |
| `certManager.namespace`          | `cert-manager`                 | Namespace where cert-manager is installed |
| `certManager.serviceAccountName` | `cert-manager`                 | cert-manager ServiceAccount name |
| `resources`                      | `{}`                           | Pod resources  |
| `nodeSelector`                   | `{}`                           | Node selector  |
| `tolerations`                    | `[]`                           | Tolerations    |
| `affinity`                       | `{}`                           | Affinity rules |

### Solver config (in Issuer/ClusterIssuer)

| Field                 | Description                                         |
|-----------------------|-----------------------------------------------------|
| `zoneUID`             | UUID of the Nubes DNS zone                          |
| `tokenSecretRef.name` | Name of the Kubernetes Secret holding the API token |
| `tokenSecretRef.key`  | Key within the Secret (e.g. `token`)                |

