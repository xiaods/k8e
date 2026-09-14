# KIP-27: Bundle cert-manager for certificate lifecycle management

**Status:** Implemented

## Default installation

K8E now deploys cert-manager **v1.21.2** by default. The server embeds
`manifests/cert-manager.yaml`, stages it into its manifests directory, and
reconciles it through the existing Addon controller. There is no install-time
Helm download or additional shell installer.

The bundle installs the certificate CRDs, RBAC, admission webhooks, Services,
and three Deployments in the `cert-manager` namespace:

- `cert-manager` (controller)
- `cert-manager-cainjector`
- `cert-manager-webhook`

HTTP-01 challenges create temporary Pods using the `acmesolver` image. All four
official `quay.io/jetstack/cert-manager-*` images are pinned to `v1.21.2` in
`hack/airgap/image-list.txt`. The existing release workflow runs
`hack/package-airgap.sh`, which pulls and saves that list. The Helm-only
`startupapicheck` hook is not deployed by this static manifest and is not needed
in its image archive.

The default adds three controller Pods and cluster-scoped RBAC/webhooks. Budget
resources for these alongside the Kubernetes control plane and sandbox pool.
It applies to upgraded servers as well as fresh installs. Agent-only installs
do not stage server manifests.

## Verify and opt out

After starting the server:

```sh
k8e kubectl -n cert-manager rollout status deployment/cert-manager
k8e kubectl -n cert-manager rollout status deployment/cert-manager-cainjector
k8e kubectl -n cert-manager rollout status deployment/cert-manager-webhook
k8e kubectl get crd certificates.cert-manager.io issuers.cert-manager.io
```

For a cluster that already has its own cert-manager installation, opt out
**before upgrading or starting K8E**, to avoid two installers owning the same
cluster-scoped resources. Use the existing bundled-addon switch:

```sh
k8e server --disable=cert-manager
```

Or merge `cert-manager` into the existing disable list in `/etc/k8e/config.yaml`:

```yaml
disable:
  - cert-manager
```

This is an uninstall switch, not a controller pause: on an existing K8E-managed
installation the Addon controller removes managed resources, including the
CRDs. Back up certificate resources and Secrets before uninstalling; deleting
CRDs also deletes their custom resources. Removing the switch reinstalls the
bundle, but does not restore deleted certificate configuration. Do not delete
the staged manifest as an opt-out; the server stages it again on startup.

## Certificate issuance remains explicit

Installing cert-manager does not create an Issuer, ACME account, or Certificate,
change DNS/firewall rules, or contact a public CA to request a certificate. It
does not grant clients access to sandboxes. The existing gRPC mTLS CA and client
credential flow are unchanged.

For public HTTPS, an operator still supplies a domain and chooses an Issuer and
challenge method. Follow the upstream [Gateway HTTP-01 configuration guide](https://cert-manager.io/docs/configuration/acme/http01/#configuring-the-http-01-gateway-api-solver):

1. Ensure the Gateway API CRDs exist, then enable Gateway API support in the
   controller configuration and restart the controller. The vendored manifest
   retains upstream defaults; Gateway support is not implicitly enabled.
2. Point DNS at the public gateway address and make port 80 reachable for HTTP-01.
3. Create an Issuer and Certificate in the sandbox namespace. Reference the
   existing `e2b` Gateway's `http` listener from the HTTP-01 solver.
4. Set the Certificate's `secretName` to `sandbox-e2b`, which the Gateway's
   HTTPS listener already references. Bind the intended HTTPRoutes to the
   `https` listener as well; a certificate alone does not attach HTTP routes.
5. Check `Certificate Ready=True`, the Gateway listener/route conditions, and
   an HTTPS request with certificate verification enabled.

cert-manager renews configured certificates automatically. Disconnected
installations have the required container images available, but public ACME
issuance still needs outbound CA access and inbound challenge validation;
use an appropriate private issuer when fully offline. For registry mirrors,
configure containerd's `quay.io` mirror: the vendored manifest uses the official
fully qualified image names.

## Source and maintenance

`manifests/cert-manager.yaml` is the unmodified official release asset:

- Source: https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml
- SHA-256: `e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f`

For an upgrade, replace the upstream asset, update the four airgap image pins,
regenerate embedded resources using the project generator, and run
`go test ./pkg/deploy`. The tests cover default staging, the disable transition,
CRD/controller completeness, and image archive coverage (including the solver
image passed as a controller argument).

The existing manifest/AddOn boundary is sufficient; this change introduces no
new installer, certificate authority, or reconciliation loop.
