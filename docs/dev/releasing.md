# Releasing Substrate

A release is a `vX.Y.Z` tag, or `vX.Y.Z-rc.N` for a release candidate, with container images in `ghcr.io/<owner>/<repository>` and a GitHub release whose notes are generated from the merged pull requests. The workflow publishes artifacts for `agent-substrate/substrate` and `kagent-dev/substrate`. How the notes are generated, and how to label and title a pull request for them, is in [Release notes](release-notes.md).

## Who may release

Pushing a `vX.Y.Z` or `vX.Y.Z-PRERELEASE` tag publishes that commit's images to the repository's GHCR namespace and creates a draft release, under the repository's token. Whoever can push such a tag can therefore publish images for any commit. Only release managers may push one.

A tag ruleset on `agent-substrate/substrate` that restricts creating, updating, and deleting `v[0-9]*` tags to release managers enforces this for tag pushes. An organization admin maintains it, and it has to be in place before the first release that publishes images.

The ruleset limits tag pushes only. Anyone with write access can still publish images: they can start this workflow from the Actions tab, from any branch, and publish or overwrite the images of a tag whose release is still a draft, and a workflow on a branch they push can request `packages: write` as well. Once a tag's release is published, this workflow publishes nothing for it.

## Cut a release

1. Tag the tested commit on the intended release branch and push the tag. Both tag pushes and manual runs of the [`release`](../../.github/workflows/release.yaml) workflow check out that tag:
   1. The `images` job runs `ate-setup publish release-images`, which builds every release image for `linux/amd64` and `linux/arm64`, pushes it to `ghcr.io/<owner>/<repository>/<image>:<tag>`, and then checks that every tag names the image just pushed, for both platforms. The ko images and `envoy-dataplane`, built from its Dockerfile with `docker buildx`, all cross-compile for arm64 without emulation.
   2. In `kagent-dev/substrate`, the `charts` job publishes all three Helm charts after the images succeed. See [Fork Helm charts](#fork-helm-charts).
   3. Once artifact publishing succeeds, the `draft` job builds the `kubectl-ate` binaries with `ate-setup publish release-binaries`, then runs [`hack/release/draft-release.sh`](../../hack/release/draft-release.sh), which creates a draft release, and attaches the binaries to it. The notes cover every change since the previous `vX.Y.Z` release, say where the release images are published and link to the list below, give the command that installs them, give the steps to check and install `kubectl-ate`, and end with a list of committers. A release candidate is marked as a prerelease.

   If images, charts, or the binary build fails, no new draft is created. If attaching the binaries fails, the draft exists without them. Either way, fix the cause and run the workflow again from the Actions tab with the tag as input.
2. Edit the draft:
   - Write a summary at the top.
   - Rewrite each breaking change with its upgrade step.
   - Optionally, group the feature sections under one `## Features` heading.
   - Remove Other Changes entries users do not need, and move entries that landed in the wrong section.
3. Publish the release.

If the workflow did not run, start it from the Actions tab with the tag as input, or run the script with an authenticated `gh` that has write access to the repository:

```sh
hack/release/draft-release.sh --dry-run v0.3.0   # print the notes only
hack/release/draft-release.sh v0.3.0             # create the draft
```

The script never modifies a release that already exists.

To preview the notes before tagging, run the dry run with the tag you plan to push. If the tag does not exist yet, the notes run up to `origin/main` in your clone, which must already be in the repository.

## Release images

A release publishes `ate-idp-server`, `ateapi`, `atecontroller`, `atelet`, `atenet`, `ateworker-gvisor`, `ateworker-microvm`, `kubernetes-secrets` (the Kubernetes Secrets credential provider), `objectstore-plugin`, and `podcertcontroller` (`images.ReleaseComponents` in [`cmd/ate-setup/internal/images`](../../cmd/ate-setup/internal/images/images.go)), plus `envoy-dataplane` from [`cmd/dataplane/envoy`](../../cmd/dataplane/envoy), as `ghcr.io/<owner>/<repository>/<image>:<tag>`. The object storage plugin ships with releases until it moves out of this repository.

Both lists are allowlists in `images.go`: `released` for the ko images and `releasedDockerfiles` for the images built from a Dockerfile. A new image is not published until a pull request adds it there, so publishing an image is always an explicit, reviewed change.

It leaves out the demos and the benchmarking fakes, which an install deploys only when asked to. An install that needs them has to take them from another registry, or build them from source. `ate-setup --image-repo` looks those images up like any other; if one is missing, the error says releases do not publish it.

Tags that begin `vX.Y.Z` start the workflow. `ate-setup` refuses any version that is not `vX.Y.Z` or `vX.Y.Z-PRERELEASE` before building, so a tag like `v1.2.3foo` publishes nothing.

`agent-substrate/substrate` and `kagent-dev/substrate` publish images and binaries.
Other forks create a draft without artifacts, since GitHub bills package storage
to the fork's owner. The image publisher accepts `--source-url` or
`ATE_PUBLISH_SOURCE_URL` to set the OCI source label. The workflow sets it to the
repository's GitHub URL so GHCR links packages to the correct repository.

The kagent-dev fork uses this workflow for every maintained release branch.
The previous `release-fork` workflow has been removed. Manual runs now require
an existing release tag and create a draft after publishing artifacts; they do
not generate branch-and-commit image tags. Image tags are versioned, and the
workflow does not move `latest`. Do not copy upstream tags into the fork as part
of branch synchronization: matching tag pushes trigger publishing. The workflow
and publisher changes must be present in a release's tagged commit.

Keep downstream tags on downstream commits and record their upstream base in the
release notes. Suffixed tags are prereleases; the notes start at the newest
unsuffixed `vX.Y.Z` tag below the release's version. Check that baseline when
releasing from multiple branches.

The separate [`agentgateway-image`](../../.github/workflows/agentgateway-image.yaml)
workflow builds AgentGateway from a selected source revision. The Substrate
release workflow does not rebuild that dependency or change the chart's image pin.

A rerun builds nothing if every image already exists for the tag on both platforms. An incomplete set, left by a failed run, is rebuilt in full. To rebuild and replace a complete set, run the workflow manually with `overwrite` checked. Replacing images changes their digests, so once the tag has a published (non-draft) release the `images` job publishes nothing and fails, with or without `overwrite`.

To test the image build without publishing anything, for example in a fork, run the workflow manually with `dry_run_images` checked. The images are pushed to a registry that exists only inside the job and are checked there. No draft is created.

GitHub creates a new package as private. After the first release that adds an image, an owner of the publishing organization must make the package public in its package settings, otherwise `ate-setup --image-repo` cannot read it without credentials.

To publish images by hand, check out the tag in a clean clone, log in to the registry with `docker login`, and run:

```sh
VERSION=v0.3.0 KO_DEFAULTPLATFORMS=linux/amd64,linux/arm64 \
  go run ./cmd/ate-setup publish release-images --ko-docker-repo <registry path>
```

For a fork, also pass `--source-url https://github.com/<owner>/<repository>`.
It defaults to the upstream URL for local builds.

The images are built from the work tree, so it must hold the tag and nothing else. The build needs Go, `ko` (which `ate-setup` runs from `hack/tools/ko`), and `docker buildx` for `envoy-dataplane`. A multi-platform push needs a builder that supports it, such as one made with `docker buildx create --driver docker-container --use`; the default `docker` driver supports it only with the containerd image store.

## Release binaries

The `draft` job attaches `kubectl-ate` for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`, as raw binaries named `kubectl-ate-<os>-<arch>`, each with a `<name>.sha256` file. There is no Windows build. The binaries are static (`CGO_ENABLED=0`) and stamped with the tag, the commit, and the commit time, which `kubectl ate --version` prints.

`agent-substrate/substrate` and `kagent-dev/substrate` attach binaries. A rerun replaces the binaries only while the release is still a draft.

To check and install one:

```sh
shasum -a 256 -c kubectl-ate-linux-amd64.sha256
install -m 0755 kubectl-ate-linux-amd64 ~/.local/bin/kubectl-ate
```

To build them by hand, check out the tag in a clean clone and run:

```sh
VERSION=v0.3.0 go run ./cmd/ate-setup publish release-binaries
```

## Fork Helm charts

The fork publishes `substrate-crds`, `substrate`, and `substrate-podcert` to
`oci://ghcr.io/kagent-dev/substrate/helm`. Their chart version is the release tag
without `v`; their `appVersion` is the full tag, which selects the matching
component images. All charts are packaged before any chart is pushed. A failed
push may leave a partial set; rerun the workflow while the release is a draft.
The chart publisher refuses writes once the release is published, including
when `overwrite` is set for images.
