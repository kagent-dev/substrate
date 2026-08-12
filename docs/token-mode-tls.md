# Token-mode TLS credentials

`--ateapi-client-auth=token` does not require the Kubernetes PodCertificate or
ClusterTrustBundle APIs. The installer creates these resources in `ate-system`:

- Secret `token-mode-ca`, key `pool`: the private signing CA. Workloads do not mount it.
- ConfigMap `token-mode-ca`, keys `ca.crt` and `trust-bundle.pem`: the public trust bundle.
- Secret `token-mode-tls`, keys `tls.crt`, `tls.key`, and `credential-bundle.pem`: one shared multi-SAN transport identity.
- Secret `valkey-auth`, key `password`: Valkey client authentication. Pass
  `--valkey-auth-secret=NAME` to use an existing Secret instead; installation
  fails when that Secret or its `password` key is missing.

WorkerPool Pods run outside `ate-system`, so Kubernetes cannot mount these
namespace-scoped resources directly. Before creating a token-mode WorkerPool,
copy `token-mode-tls` and `token-mode-ca` into its namespace with the same
names. `hack/copy-token-mode-credentials.sh NAMESPACE` is provided for local
demos; the controller does not copy credentials across namespace boundaries.
Anyone who can read that namespace's shared TLS Secret can impersonate a
token-mode TLS endpoint; use only namespaces within the deployment's trust boundary.

Router tokens are verified offline by worker atunnel servers to keep the
Kubernetes API out of the actor request path. A deleted router Pod's projected
token can therefore be replayed until its ten-minute expiry (plus clock skew).
Keep the projected lifetime short and treat the router Pod as part of the
control-plane trust boundary.

The installer never replaces an existing CA or TLS Secret. To rotate them,
create replacement resources, update the affected mounts, and roll out
ate-api-server, atelet, atenet-router, atenet-egress, Valkey, and WorkerPool
Deployments. Keep the old CA in trust bundles until every serving certificate
and client has moved to the replacement.
