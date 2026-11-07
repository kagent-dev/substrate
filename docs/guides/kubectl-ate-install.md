---
title: "Install with kubectl ate"
linkTitle: "Install with kubectl ate"
weight: 2
description: >
  Install, upgrade and uninstall Agent Substrate from its Helm charts with kubectl ate.
---

# Install with kubectl ate

`kubectl ate install` installs the `substrate-crds`, `substrate-podcert`,
`substrate-atelet` and `substrate` Helm releases in order. It also creates the
CA pools, identity secrets and object store credentials the charts expect. The
charts are built into `kubectl ate`, so no checkout is needed. Pass
`--chart-dir ./charts` to install from a checkout instead.

On a terminal, install, upgrade and uninstall draw a live step list. Pass
`--no-tui` or `-v` for plain lines. A bare `kubectl ate install` on a terminal
asks for the options below and prints the matching command.

## Evaluate on kind

```bash
kubectl ate install
kubectl ate status
```

This runs a single-replica PostgreSQL and a RustFS bucket in `ate-system`. The
PostgreSQL has no backups or high availability. Don't use it for anything you
want to keep.

Add `--microvm` to also install the microvm sandbox class. See
[MicroVM assets](#microvm-assets).

## External PostgreSQL

Provision the database, roles and both connection Secrets as
[PostgreSQL setup](../../charts/substrate/README.md#postgresql-setup) and the
[database guide](postgres.md#byo-db-configuration) describe. Create the Secrets
in `ate-system`, then:

```bash
kubectl ate install --postgres external
```

Install checks that both Secrets exist before it installs the `substrate`
release. For Cloud SQL, see
[cloud-sql.md](https://github.com/ai-on-gke/substrate-gke/blob/main/tools/setup-gcp/cloud-sql.md)
in substrate-gke.

## S3 or GCS snapshots

Pass `--object-store s3` or `gcs` with a values file. Install passes the file to
every release except `substrate-crds`.

```yaml
# gke.yaml
ateApi:
  snapshotLocation: gs://my-snapshots/
objectStorePlugin:
  transferServiceAccount: ate-snapshot-transfer@my-project.iam.gserviceaccount.com
```

```bash
kubectl ate install --postgres external --object-store gcs -f gke.yaml
```

For S3, set `objectStore.region` and, for a static key, `objectStore.credentialsSecret`
to a Secret in `ate-system` with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.
Install fails before changing any release if `ateApi.snapshotLocation`, the
GCS transfer service account, or the named Secret is missing.

On GKE, create the buckets and IAM grants with `tools/setup-gcp` in
[substrate-gke](https://github.com/ai-on-gke/substrate-gke/blob/main/tools/setup-gcp/README.md)
first. [Snapshot storage](operator-install.md#snapshot-storage) explains the
transfer service account.

## MicroVM assets

`--microvm` downloads the Kata kernel and rootfs, virtiofsd and Cloud
Hypervisor for each node architecture. It caches them under the user cache
directory, uploads the missing ones to `microvm-assets/<arch>/<sha256>` in the
object store, and applies the `microvm` SandboxConfig.

With RustFS, the assets go to the RustFS bucket through a port-forward. With S3
or GCS, pass `--microvm-asset-bucket` and make sure atelet can read that bucket.
The upload uses your own AWS or Google credentials.

## Upgrade

```bash
kubectl ate upgrade --dry-run
kubectl ate upgrade
```

Upgrade moves the certificate controller, atelet, the CRDs, then the
`substrate` release to the charts in this `kubectl ate`. It doesn't roll
WorkerPool worker images, which the
[version skew policy](../reference/version-skew-policy.md) upgrades after
atelet. Each release keeps the values it was installed with. `-f` and `--set` are layered on
top. Upgrade refuses downgrades, skipped minor releases, and CRD changes that
would strand stored objects. If the `microvm` SandboxConfig exists, upgrade
stages the assets the new release pins and reapplies it.

`--dry-run` prints each release's manifest diff and changes nothing.

## Uninstall

```bash
kubectl ate uninstall
```

Uninstall deletes every WorkerPool and waits for ate-controller to finalize
them. Then it uninstalls the releases in reverse order and removes the bundled
PostgreSQL. It asks first. Pass `--yes` in scripts.

CRDs, CA keys, secrets, the database volume and the RustFS volume stay, so a
later install keeps its identities and data. `--include-crds` also deletes the CRDs, with every
WorkerPool, SandboxConfig and CSIDriverConfig. `--purge` also deletes the
namespaces, secrets, database volume and trust bundles.
