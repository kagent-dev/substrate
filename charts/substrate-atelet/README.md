# substrate-atelet

Install the atelet DaemonSet as a release separate from the
[substrate](../substrate/README.md) chart, so it can be upgraded on its own
schedule. The substrate chart renders the same atelet when `atelet.enabled` is
true, which is its default.

Install the substrate chart with `atelet.enabled=false`, then install this
chart in the same namespace:

```sh
helm upgrade --install substrate ./charts/substrate \
  --namespace ate-system --create-namespace \
  -f values.yaml --set atelet.enabled=false
helm upgrade --install substrate-atelet ./charts/substrate-atelet \
  --namespace ate-system \
  -f values.yaml --wait
```

This chart reads the same value paths as the substrate chart: `atelet`,
`objectStorePlugin.node`, `objectStorePlugin.extraEnv`, `preview`, `otel`,
`rustfs.enabled` and its credentials, `image`, `imagePullSecrets`, and `global`.
Pass the substrate release's values file to both releases so the settings
match.

Set `substrateReleaseName` to the substrate release's name. It defaults to
`substrate`. atelet's objects take their names from it, because the substrate
release grants permissions to the atelet ServiceAccount by name. atelet also
uses it to reach that release's API server.

## Moving atelet out of an existing substrate release

Upgrading the substrate release with `atelet.enabled=false` deletes atelet.
To move atelet to this chart without restarting it, keep the objects through
that upgrade, then hand them to the new release:

```sh
# Runs a kubectl verb on every object the atelet template renders.
objects() {
  local verb="$1"; shift
  kubectl -n ate-system "${verb}" serviceaccount/atelet daemonset/atelet \
    role/atelet-endpointslices rolebinding/atelet-endpointslices "$@"
  kubectl "${verb}" clusterrole/atelet-role clusterrolebinding/atelet-binding \
    priorityclass/ate-node-critical "$@"
}

# 1. Keep the objects when the substrate release stops rendering them.
objects annotate --overwrite helm.sh/resource-policy=keep
# 2. Stop managing atelet in the substrate release.
helm upgrade substrate ./charts/substrate -n ate-system \
  -f values.yaml --set atelet.enabled=false
# 3. Hand the objects to the new release. Helm refuses to install over
#    objects that name another release.
objects annotate --overwrite meta.helm.sh/release-name=substrate-atelet
# 4. Install the new release. It adopts the objects.
helm upgrade --install substrate-atelet ./charts/substrate-atelet \
  -n ate-system -f values.yaml --wait
# 5. Remove the keep policy so later upgrades can delete the objects.
objects annotate helm.sh/resource-policy-
```

Names other than `substrate` prefix each object with the release name, for
example `team-atelet`. If `atelet.imageCredentialProviderConfigYAML` is set,
also move `configmap/atelet-image-credential-provider`.

Use the same values for both releases. Then the DaemonSet's pod template does
not change and the atelet pods keep running.

To move atelet back, let the substrate release adopt the objects, then delete
this release's records. Do not run `helm uninstall substrate-atelet`. It
deletes atelet even when the objects carry `helm.sh/resource-policy: keep`.

```sh
objects annotate --overwrite meta.helm.sh/release-name=substrate
helm upgrade substrate ./charts/substrate -n ate-system -f values.yaml
kubectl -n ate-system delete secret -l owner=helm,name=substrate-atelet
```
