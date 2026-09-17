# Kubernetes credential provider

The optional `k8s-credential-provider` Deployment follows the provider from
[upstream](https://github.com/agent-substrate/substrate/pull/1335). It serves
`CredentialProvider.FetchSecret` at `k8s-credential-provider.ate-system.svc:50051`
with its own ServiceAccount and projected serving certificate. AGW calls it
directly over mTLS on the HTTPS interception route.

`ate-secret://kubernetes.io/team-a-secrets/example-api/token` resolves the `token`
entry in that Kubernetes Secret. Omitting the key requires exactly one data entry.
The provider reads Kubernetes on every fetch and never persists or logs values.
AGW caches successful credentials per actor and URI for five minutes, so rotation
can take that long to reach injected requests.

Each request requires a trusted injector certificate with the configured SPIFFE
identity, an explicit atespace-to-namespace grant for the attested actor, and
Kubernetes `get` permission for the provider's ServiceAccount. Empty policies
deny all requests. Secret permissions are granted separately from ateapi.

## Enable the provider

Keep the pinned `images.agentgateway` image. It includes the
[protocol update](https://github.com/agentgateway/agentgateway/pull/3524) from
[this build](https://github.com/agentgateway/agentgateway/actions/runs/35238449333)
and implements the current [FetchSecret contract](../pkg/proto/credproviderpb/credprovider.proto).

Create the MITM CA Secret using the existing installation tooling:

```sh
hack/install-ate-kind.sh --create-egress-mitm-ca-pool-secret
```

The gateway needs `egress-mitm-ca-pool` with `tls.crt` and `tls.key` in its namespace.
Actors must trust this CA; see the [MITM trust bundle guide](egress-trust-bundle.md).

For Helm, add these values to your release configuration:

```yaml
credentialProvider:
  enabled: true
  namespacePolicies:
  - atespace: team-a
    allowedNamespaces: [team-a-secrets]
```

The feature is disabled by default. Enabling it deploys the provider and configures
AGW's HTTPS interception route. Cleartext egress cannot receive injected Secrets.
Policy changes roll the provider's Pods. Resource names and the injector identity
follow the release: release `demo` in namespace `platform` uses ServiceAccount
`demo-k8s-credential-provider`, endpoint
`demo-k8s-credential-provider.platform.svc:50051`, and injector identity
`spiffe://cluster.local/ns/platform/sa/demo-atenet-egress`.

For the manifest installer, set your grants in
`manifests/egress-credential-injection/namespace-policy.yaml`, then deploy:

```sh
kubectl kustomize manifests/egress-credential-injection | ko apply -f -
hack/install-ate.sh --deploy-atenet \
  --atenet-dataplane=agentgateway --experimental-use-sdsmint
```

The policy file uses `policies:` with the same list of grants as the Helm values.
Its generated ConfigMap name changes with the policy, rolling the provider on
reapplication. Direct ConfigMap edits require a rollout restart: policy and client
CA files are loaded at startup. Serving certificates rotate through the existing
certificate loader.

## Grant Secret access

Create the Secret, then bind only the required reads to the provider:

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
  name: k8s-credential-provider
  namespace: ate-system
```

For a named Helm release, use its prefixed provider ServiceAccount and namespace.
Neither installer grants cluster-wide Secret access.

Set an actor's egress policy header injection to use credential URI
`ate-secret://kubernetes.io/team-a-secrets/example-api/token`, header
`authorization`, and prefix `Bearer `. Namespace grants alone do not create an
egress policy. No ext_proc injector is needed.

## Tests

The Helm PR workflow runs `internal/e2e/suites/credentials` with real actors,
Secrets, RBAC, projected certificates, AGW, and the deployed provider. It checks
the exact injected token, an unauthenticated-origin control, namespace-policy
denial, cache isolation between atespaces, Kubernetes RBAC denial, and cleartext
denial. SubjectAccessReviews verify
the permission assumptions.

On a dedicated Helm-installed Kind cluster with this branch's images, including
`kubernetes-secrets`, and the MITM CA Secret:

```sh
helm upgrade substrate charts/substrate --namespace ate-system \
  --reuse-values -f internal/e2e/suites/credentials/values.yaml --wait --timeout=5m
E2E_ATENET_DATAPLANE=agentgateway E2E_CREDENTIAL_PROVIDER=1 \
  hack/run-e2e-kind.sh ./internal/e2e/suites/credentials -v -args --no-color
```

Enabling interception changes cluster egress TLS, so run this after tests that
require passthrough. The test temporarily trusts the cluster serving CA for its
local HTTPS origin and restores gateway configuration on cleanup.
