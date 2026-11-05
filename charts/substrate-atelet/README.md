# substrate-atelet

Install the atelet DaemonSet as a release separate from the
[substrate](../substrate/README.md) chart, so it can be upgraded in the order
the [version skew policy](../../docs/reference/version-skew-policy.md) requires:
atelet before workers, and workers before ate-api-server.

The substrate chart also renders atelet when `atelet.enabled` is true, which is
its default. That mode is deprecated, because one upgrade of the substrate
release moves atelet and ate-api-server together. New installs should use this
chart.

## Install

Install this chart first, then the substrate chart with `atelet.enabled=false`
in the same namespace. Upgrade them in the same order. With the substrate
chart's bundled RustFS:

```sh
kubectl create namespace ate-system
kubectl create secret generic rustfs-credentials -n ate-system \
  --from-literal=AWS_ACCESS_KEY_ID=rustfsadmin \
  --from-literal=AWS_SECRET_ACCESS_KEY=rustfsadmin
helm upgrade --install substrate-atelet ./charts/substrate-atelet \
  --namespace ate-system \
  -f values.yaml \
  --set objectStore.endpoint=http://rustfs.ate-system.svc:9000 \
  --set objectStore.region=us-east-1 \
  --set objectStore.pathStyle=true \
  --set objectStore.credentialsSecret=rustfs-credentials \
  --wait
helm upgrade --install substrate ./charts/substrate \
  --namespace ate-system \
  -f values.yaml --set atelet.enabled=false
```

Use the credentials from the substrate release's `rustfs.accessKey` and
`rustfs.secretKey`.

## Values

This chart reads the same value paths as the substrate chart for `atelet`,
`objectStorePlugin.node`, `objectStorePlugin.extraEnv`, `preview`, `otel`,
`image`, `imagePullSecrets` and `global`, so the substrate release's values
file can configure both releases.

Object storage is set under `objectStore` instead of `rustfs` and
`atelet.storageBackend`:

| Key | Default | Description |
|---|---|---|
| `objectStore.backend` | `s3` | Snapshot backend, `s3` or `gcs` |
| `objectStore.endpoint` | `""` | S3 endpoint URL. Empty uses the AWS endpoint for the region |
| `objectStore.region` | `""` | S3 region |
| `objectStore.pathStyle` | `false` | Address buckets by path rather than by virtual host |
| `objectStore.credentialsSecret` | `""` | Secret with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` keys. Empty uses the SDK's default credential chain |

Set `substrateReleaseName` to the substrate release's name. It defaults to
`substrate`. atelet's objects take their names from it, because the substrate
release grants permissions to the atelet ServiceAccount by name. atelet also
uses it to reach that release's API server. Names other than `substrate`
prefix each object with the release name, for example `team-atelet`.

## Existing installs

Upgrading the substrate release with `atelet.enabled=false` deletes atelet.
Switch an existing install to this chart when you next reinstall, which every
minor upgrade before 1.0 requires.
