# substrate-podcert

Install the Substrate certificate controller once per cluster, independently
of the application release. It issues service DNS and pod identity certificates
and publishes their cluster trust bundles. It does not require PostgreSQL or
a certificate issued by itself.

The cluster must enable `ClusterTrustBundle`, `ClusterTrustBundleProjection`,
and `PodCertificateRequest`, including the `certificates.k8s.io/v1beta1` API.
Create `service-dns-ca-pool` and `pod-identity-ca-pool` Secrets in the controller's
namespace before installation. Each Secret must contain a signing pool in its
`pool` key. The existing setup command creates these in the default namespace:

```sh
go run ./cmd/ate-setup create podcertificate-controller-cas
helm upgrade --install substrate-podcert ./charts/substrate-podcert \
  --namespace podcertificate-controller-system --create-namespace \
  --wait --timeout=5m
hack/install-postgres.sh --kind
```

Then prepare the application authentication resources and install the Substrate
application chart. The controller's namespace is the Helm release namespace;
if selecting another namespace, provision both CA-pool Secrets there.

Configure the controller image through `image.registry`, `image.repository`,
and `image.tag`. `global.imageRegistry` overrides the registry, and
`imagePullSecrets` and `global.imagePullSecrets` are merged for the controller pod.

For an existing installation, the controller resources must be transferred out
of the application release before the new release can own them. Installing this
chart over resources still owned by the application release fails Helm ownership
validation. Perform the release split during a planned installation update;
do not delete the existing CA-pool Secrets.
