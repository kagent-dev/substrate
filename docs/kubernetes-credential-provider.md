# Kubernetes credential provider

The optional `k8s-credential-provider` sidecar runs in ateapi's Pod. It serves
`CredentialProvider.FetchSecret` on `api.ate-system.svc:50051`, using ateapi's
serving certificate and ServiceAccount. ateapi's own gRPC service stays on 443.
No additional Deployment or ServiceAccount is needed.

A URI such as `ate-secret://kubernetes.io/team-a-secrets/example-api/token`
resolves the `token` entry in that Kubernetes Secret. Omitting the key is allowed
only for a Secret with exactly one data entry. Each request reads Kubernetes,
so Secret rotation is visible on the next provider fetch. AGW currently caches
successful credentials per actor and URI for five minutes, so injection may
continue using a cached value until it expires.
Secret values are neither persisted nor logged by the provider.

Access requires all three checks:

- The caller presents a trusted mTLS certificate with the configured egress
  injector SPIFFE identity. A different trusted workload is still rejected.
- The actor SPIFFE identity attested by that injector belongs to an atespace
  explicitly granted access to the Secret's namespace. Empty policies deny all.
- ateapi's ServiceAccount has Kubernetes `get` permission on that Secret.

The sidecar shares ateapi's Kubernetes identity and Pod failure domain. It is
process separation, not a separate Kubernetes authorization boundary.

## Agentgateway compatibility

The pinned AGW nightly includes the
[credential protocol update](https://github.com/agentgateway/agentgateway/pull/3524)
from [this build](https://github.com/agentgateway/agentgateway/actions/runs/35238449333).
It implements the current Substrate credential protocol:

- RPC: `/credprovider.CredentialProvider/FetchSecret`.
- Request: `uri` (field 1), `actor_spiffe_id` (field 2).
- Response: `opaque_bytes` (field 1).
- URI scheme: `ate-secret://`.

The contract is [credprovider.proto](../pkg/proto/credproviderpb/credprovider.proto).
The provider does not implement the old `RequestSecret` RPC or
`substrate-secret://` scheme.

## Enable the sidecar

First create the MITM CA Secret using the existing installation tooling:

```sh
hack/install-ate-kind.sh --create-egress-mitm-ca-pool-secret
```

The Secret is named `egress-mitm-ca-pool` and must contain `tls.crt` and `tls.key`
in the gateway's namespace. For a non-default namespace, provision the same
Secret there. Actors must trust this CA; see the
[MITM trust bundle guide](egress-trust-bundle.md).

For Helm, keep the pinned `images.agentgateway` image and add these values to
your existing release configuration:

```yaml
ateApi:
  credentialProvider:
    enabled: true
    namespacePolicies:
    - atespace: team-a
      allowedNamespaces: [team-a-secrets]
```

The chart derives the injector identity and Service name from the release name
and namespace. For example, release `demo` in namespace `platform` serves at
`demo-api.platform.svc:50051` and accepts only
`spiffe://cluster.local/ns/platform/sa/demo-atenet-egress`.
Enabling it also configures AGW's HTTPS interception route to fetch credentials
from the sidecar over mTLS. Credential providers are configured only on the HTTPS
route, so cleartext egress cannot receive injected Secrets. The feature is
disabled by default. Policy changes through Helm roll the ateapi
Pods so each sidecar loads the new policy.

For the manifest installer, add
`manifests/ate-install/components/credential-provider` to your existing ateapi
Kustomization's `components`. Set the grants in its `policy.yaml`:

```yaml
policies:
- atespace: team-a
  allowedNamespaces: [team-a-secrets]
```

A ready-made overlay of the repository's ateapi defaults is also available:

```sh
kubectl kustomize --load-restrictor=LoadRestrictionsNone \
  manifests/ate-install/kubernetes-credentials | ko apply -f -
```

Preserve any existing installation-specific ateapi patches in your overlay.
The component's generated ConfigMap name changes with its policy, rolling the
Pods on reapplication. Direct edits to a mounted ConfigMap require a rollout
restart: the policy and client CA bundle are loaded at startup. The serving
credential bundle follows the existing certificate loader's rotation behavior.

## Grant Secret access

Create the Secret in `team-a-secrets`, then grant only the required Secret reads.
For the default installation, this Role and RoleBinding allow the example above:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: egress-credentials
  namespace: team-a-secrets
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: [example-api]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: egress-credentials
  namespace: team-a-secrets
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: egress-credentials
subjects:
- kind: ServiceAccount
  name: ate-api-server
  namespace: ate-system
```

For a named Helm release, use its prefixed ateapi ServiceAccount and namespace.
Neither the chart nor the overlay grants cluster-wide Secret access.

## Connect agentgateway

The Helm configuration above wires `substrateEgress.credentialProviders` to the
sidecar on the HTTPS interception route. For the manifest installer, keep the
pinned AGW image, install the sidecar overlay, and deploy the AGW MITM
configuration:

```sh
hack/install-ate.sh --deploy-atenet \
  --atenet-dataplane=agentgateway \
  --experimental-use-sdsmint
```

The AGW MITM component points `uriAuthority: kubernetes.io` at
`api.ate-system.svc:50051`, using its existing pod identity certificate and
service DNS CA bundle. No ext_proc injector is needed.

Set an egress policy header injection's credential URI to
`ate-secret://kubernetes.io/team-a-secrets/example-api/token`, for example with
header `authorization` and prefix `Bearer `. Namespace grants alone do not
create an egress policy.

## Validate an AGW image locally

On Linux with a local Docker daemon and Helm installed, set
`AGENTGATEWAY_TEST_IMAGE` to the image reference from `images.agentgateway` in
`charts/substrate/values.yaml`, then run:

```sh
go test -race ./cmd/k8s-credential-provider -run '^TestAgentgatewayInjection$' -count=1 -v
```

The test skips unless that environment variable is exported. It uses the
rendered Helm egress configuration, an authenticated actor CONNECT tunnel, TLS
interception, the real credential-provider server over mTLS, and a TLS upstream.
It verifies header injection, denial for an ungranted atespace after a successful
fetch, and denial of credential injection on cleartext egress. Kubernetes Secret
storage and ateapi's actor/policy RPCs are faked; it does not validate cluster
RBAC, certificate provisioning, or Pod deployment.
