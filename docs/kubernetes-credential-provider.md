# Kubernetes credential provider

The `k8s-credential-provider` Deployment follows the provider from
[upstream](https://github.com/agent-substrate/substrate/pull/1335). It serves
`CredentialProvider.FetchSecret` at `k8s-credential-provider.ate-system.svc:50051`
with its own ServiceAccount and projected serving certificate. AGW calls it
directly over mTLS to inject credentials into HTTP and intercepted HTTPS requests.

`ate-secret://kubernetes.io/team-a-secrets/example-api/token` resolves the `token`
entry in that Kubernetes Secret. Omitting the key requires exactly one data entry.
The provider reads Kubernetes on every fetch and never persists or logs values.
AGW caches successful credentials per actor and URI for five minutes, so rotation
can take that long to reach injected requests.

The same Deployment serves a second provider name for Google Cloud.
`ate-secret://google-access-token.kubernetes.io/team-a-secrets/vertex/credentials.json`
resolves the same entry, requires it to hold a Google service account key in JSON
form, and returns an OAuth 2.0 access token for that service account instead of the
key. See [Google access tokens](#google-access-tokens).

Each request requires a trusted injector certificate with the configured SPIFFE
identity, an explicit atespace-to-namespace grant for the attested actor, and
Kubernetes `get` permission for the provider's ServiceAccount. Both installers
include the upstream get-only Secret ClusterRole and bind it to that ServiceAccount.
The provider can read Secrets across namespaces; its namespace policy controls
which namespaces each actor may use. Empty policies deny all requests.

## Configure the provider

Keep the pinned `images.agentgateway` image. It includes the
[protocol update](https://github.com/agentgateway/agentgateway/pull/3524) from
[this build](https://github.com/agentgateway/agentgateway/actions/runs/35238449333)
and implements the current [FetchSecret contract](../pkg/proto/credproviderpb/credprovider.proto).

Create the MITM CA Secret using the existing installation tooling:

```sh
hack/install-ate-kind.sh --create-egress-mitm-ca-pool-secret
```

The gateway needs `egress-mitm-ca-pool` with `tls.crt` and `tls.key` in its namespace.
Actors making HTTPS requests must trust this CA; see the
[MITM trust bundle guide](egress-trust-bundle.md).

For Helm, add these values to your release configuration:

```yaml
credentialProvider:
  namespacePolicies:
  - atespace: team-a
    allowedNamespaces: [team-a-secrets]
```

The Helm chart always deploys the provider and configures AGW's HTTP route and
HTTPS interception route. Namespace grants default to an empty list.
Policy changes roll the provider's Pods. Resource names and the injector identity
follow the release: release `demo` in namespace `platform` uses ServiceAccount
`demo-k8s-credential-provider`, endpoint
`demo-k8s-credential-provider.platform.svc:50051`, and injector identity
`spiffe://cluster.local/ns/platform/sa/demo-atenet-egress`.

HTTPS uses a dynamic backend: AGW selects the destination from the request and
validates its certificate using the system CA roots (`backendTLS: {}`). Public
APIs such as OpenAI and Anthropic need no per-backend certificates. The single
MITM CA lets AGW generate actor-facing certificates as needed. HTTP also travels
through the authenticated CONNECT tunnel, then leaves AGW over plaintext HTTP.

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

## Google access tokens

Google APIs, Vertex AI among them, accept OAuth 2.0 access tokens rather than
service account keys, and minting one means signing a JWT with the key's private
key. The `google-access-token.kubernetes.io` provider name does that inside the
provider, so the key stays in the cluster and actors receive a token that expires
within the hour. Set the injection header to `authorization` with prefix `Bearer `.

On each fetch the provider reads the Secret, signs the key's JWT assertion and
exchanges it at the key's `token_uri` (`https://oauth2.googleapis.com/token` for
keys Google issues) for a token scoped to
`https://www.googleapis.com/auth/cloud-platform`. The service account's IAM roles
decide what the token may do. The `token_uri` has to be an HTTPS URL, and the Pod
has to be able to reach it; the provider honors `HTTPS_PROXY` in its environment.

Tokens live one hour. The provider caches each token per key and hands it out
while at least fifteen minutes remain, which keeps AGW's five-minute cache from
serving an expired token and exchanges a key about once every 45 minutes.
Concurrent fetches of one key share a single exchange. The cache is keyed on the
Secret's contents, so a rotated key is exchanged on its next fetch. A Secret that
does not hold a service account key, or a key Google rejects, fails the fetch with
`FAILED_PRECONDITION` naming the reason; an unreachable token endpoint fails it
with `UNAVAILABLE`. The private key and the token are never logged.

Authorization is unchanged: the injector identity, the atespace-to-namespace
grant and the provider's Secret RBAC apply to both provider names. The
agentgateway data plane routes both names to this Deployment. The Envoy data
plane's `--credential-provider-name` flag names one provider, so it serves only
the name it is given.

## Configure injection

Create the Secret and set an actor's egress policy header injection to use credential URI
`ate-secret://kubernetes.io/team-a-secrets/example-api/token`, header
`authorization`, and prefix `Bearer `. Namespace grants alone do not create an
egress policy. No ext_proc injector is needed.

## Tests

The Helm PR workflow installs the provider and MITM gateway from the start and
runs `internal/e2e/suites/credentials` alongside the standard suites with real actors,
Secrets, chart-managed RBAC, AGW, and the deployed provider. It checks
the exact injected token, an unauthenticated-origin control, namespace-policy
denial and cache isolation between atespaces. The local origin serves HTTP;
the suite uses the installed gateway configuration without modifying ConfigMaps.

Include `-f internal/e2e/suites/credentials/values.yaml` in the initial Helm
installation to grant the test atespace access. After deploying the standard
MITM egress fixtures, run the suites together:

```sh
E2E_ATENET_DATAPLANE=agentgateway E2E_CREDENTIAL_PROVIDER=1 E2E_EGRESS_MITM=1 \
  hack/run-e2e-kind.sh -v -args --no-color
```

The credential suite tests HTTP injection. The existing MITM suite checks HTTPS
interception and actor trust against a public HTTPS origin using the same install.
