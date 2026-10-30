---
name: sync-upstream
description: Maintain kagent-dev/substrate main and release branches against the matching agent-substrate/substrate branches while preserving fork patches. Use for upstream syncs, establishing downstream release branches, and backporting fork fixes; not general feature-branch rebases or publishing releases.
---

# Maintain the Substrate Fork

The fork uses merge-based synchronization. Each maintained branch retains its
fork changes and receives updates from the matching upstream branch:

| Destination | Upstream source | Fork changes |
|---|---|---|
| `main` | `upstream/main` | Development and fixes |
| `release-X.Y` | `upstream/release-X.Y` | Selected compatible fixes |

Develop fork changes on `main` first, then backport applicable fixes. An urgent
release fix may start on its release branch; also apply it to `main` where
relevant. Do not merge all of fork `main` into an established release branch.
Routine syncs preserve history; they do not rebuild the fork patch stack.

Keep work local unless publication is authorized. Opening a PR requires an
explicit request or approval. Approval to open a PR does not authorize merging
it, creating release tags, or publishing artifacts. A sync request does not
authorize pushing directly to protected branches or resetting their history.

## Select the operation and revisions

1. Confirm remotes (`origin`: `kagent-dev/substrate`; `upstream`:
   `agent-substrate/substrate`), destination, worktree, and local changes.
   Preserve unrelated work in an isolated worktree under `$TMPDIR`. Check for
   an existing sync PR or another agent working on the same destination.
2. Fetch the relevant refs and record their SHAs. Pin both the fork destination
   and the selected upstream revision. For patch selection, also pin the fork
   source revision, including the inventory and merge resolutions read from it.
   Another agent advancing `main` must not silently change that patch set. Honor
   an explicit release cutoff or tag; do not substitute latest `main`. Ask for
   the destination if the request and context do not identify it. Do not treat
   every old release branch as supported.
3. Choose the operation from the requested outcome and current branch state:
   - **Routine sync:** merge the matching upstream line into an already patched
     fork branch.
   - **Establish a release:** combine the selected upstream release with our
     applicable patches, whether the destination is absent or already exists
     as an upstream-only mirror.
   - **Backport:** apply selected changes to an established release branch.
4. Read the destination's `AGENTS.md` and compatibility guidance. From 1.0,
   consult `docs/reference/version-skew-policy.md` for release changes. API
   additions and schema migrations do not belong in patch releases.

## Track the fork's patches

Use `docs/fork-patches.md` on fork `main` as the canonical inventory when it
exists. Record each maintained behavior, its source PR or commits, upstream
status, supported-branch status, and the tests that exercise it. Identify a
behavior by its purpose; cherry-picks have different SHAs across branches.

Check both ordinary fork commits and integration changes in earlier sync
merges. A list of non-merge commits alone can miss required behavior. Cover
deployment and publishing changes as well as Go code. Preserve the relevant
tests when upstream refactors the implementation.

If no inventory exists, build the needed inventory from history and the
upstream-to-fork diff before claiming all patches are preserved. Include it in
the handoff; add the canonical document when establishing patch tracking is in
scope. Do not guess a patch's purpose from its title alone.

For a patch PR, state which supported branches need it. For a sync, account for
patches affected by incoming changes and any redundant implementation removed.
Mark a patch as supplied by upstream separately for each branch, only after that
branch contains the replacement and the behavior is verified. When the task
targets a release branch, report inventory updates for `main` without modifying
or publishing another branch as an implicit side effect.

## Routine upstream sync

1. Create `sync/upstream-<destination>-YYYYMMDD` from the recorded fork tip,
   choosing a unique suffix if occupied. If the selected upstream commit is
   already an ancestor, there are no upstream commits to merge. This check does
   not establish that an upstream-only mirror contains our patches.
2. Run `git merge --no-ff --no-commit <upstream-sha>`. Resolve conflicts using
   the destination's APIs while retaining intentional fork behavior. Inspect
   automatic merges where both sides changed the same component, including
   renamed or removed APIs and all fork callers.
3. Check chart implications of manifest and CRD changes. The fork's chart uses
   agentgateway, while preserved upstream manifests may use Envoy. Translate
   applicable behavior without replacing the chart's dataplane. The render
   check excludes preserved manifests, so passing it alone does not establish
   parity. Keep `charts/substrate-crds/templates` aligned with generated CRDs.
4. Create exactly one sync merge commit on top of the recorded fork tip, using
   `git commit -s` with the configured human identity. Fold conflict resolutions
   and integration fixes into that commit with `git commit --amend -s`. Keep
   unrelated changes separate. Preserve incoming upstream commits; do not squash
   their history or replay the existing fork commits.
5. Verify `git rev-list --first-parent --count <fork-sha>..HEAD` returns `1` and
   HEAD has exactly two parents: the recorded fork tip first and selected
   upstream commit second. This counts the preparation branch, not the later
   GitHub PR merge commit.

## Establish a downstream release branch

Use upstream's selected `release-X.Y` head, including its release-specific
backports, unless the user explicitly pins a different revision. A tag and the
current release head may differ; record which one is being used.

Prefer a tested fork candidate prepared at the release cutoff. Confirm its
upstream base belongs to the release line and inspect any commits added after
that base. Reject candidates containing upstream `main` development excluded
from the selected release. Merging an older release ref cannot remove newer
commits already in a candidate's ancestry.

When a suitable candidate exists, integrate the selected upstream release into
that candidate and validate the combined result. When none exists, start at the
selected upstream release and replay the applicable fork changes in dependency
order, including integration fixes from prior syncs. Adapt them to that release
and skip changes already provided there. This is initial branch establishment,
not the routine sync procedure; it need not produce one merge commit.

If the destination already exists, preserve its tip as an ancestor and prepare
a PR against it. For an upstream-only mirror, add the fork patches through that
PR. If it already contains downstream changes, preserve and reconcile those
too. Do not reset an existing protected branch to a prepared candidate.

If the destination is absent, explicit branch-creation authorization is needed.
Prepare and validate the candidate first. A reviewable route is to create the
upstream baseline branch, then propose the downstream changes against it; report
that this branch remains an upstream-only mirror until those changes are merged.
Immediately before creating it, confirm the destination is still absent and
use a create-only operation. If another actor created it, refetch and reconcile
with that tip instead of overwriting it.

Before calling the branch ready, verify the patch inventory, CI triggers,
required checks, and release configuration for that branch. Record gaps and
prepare scoped fixes where authorized. Protection settings alone do not prove
CI runs or blocks merges. Continue `main` development separately after the
release candidate is fixed.

## Backport selected changes

Start from the recorded destination tip. Use the source patch commit, usually
the merged fork `main` PR, with `git cherry-pick -x -s <commit>`. Check dependencies
and whether upstream already supplies the behavior. Do not cherry-pick a whole
upstream sync merge as a substitute for identifying the needed fix.

Adapt conflicts to the destination's interfaces and compatibility contract.
Preserve the source reference in the PR if adaptation substantially changes the
implementation. Verify the affected behavior on the destination; passing on
`main` does not establish a valid backport. Report any remaining branch work,
including a fix that started on a release branch and is still missing on `main`.

## Validate the candidate

For an upstream sync or initial release candidate, run `go test -race ./...`,
`hack/verify-all.sh`, `hack/render-manifests.sh --check`, and
`hack/verify/crd-chart.sh` where those fork checks exist. Investigate missing
checks rather than silently treating their absence as success. Use Go's default
cache. For a narrow backport, follow the destination's required checks and add
validation appropriate to the affected behavior.

Use `.github/workflows/pr-workflow.yaml` and `.github/workflows/helm-e2e.yaml`
at the candidate revision to identify runtime coverage. Validate the fork's
agentgateway configuration on both gVisor and microVM for runtime changes and
release establishment. Local E2E uses task-owned resources and must not disrupt
shared clusters or registries. Package tests without `--e2e` are not runtime
coverage. Validate release upgrades and rollbacks against the previous supported
downstream release when required by the version-skew policy.

For failures, record the exact SHA, job, runtime, and failing test or setup step.
Read test output and component diagnostics together. Distinguish regressions
from missing prerequisites and existing failures using the base revision when
needed. Check whether incoming upstream commits already address the failure.
An unrelated green lane or an unexamined rerun does not explain a failure.

## Hand off or publish

Before publishing, refetch the destination and source. If the destination
advanced, rebuild on its new tip and revalidate. Report upstream advances;
refresh a latest-tip sync, but retain an explicitly pinned revision or cutoff.
For routine syncs, rebuild the single merge instead of stacking more merges.
Keep a PR's rebase or reconstruction push separate from fixes to reviewed code.

Review the diff and confirm it matches the operation and patch inventory. For
substantial changes, obtain an independent review and validate its findings.
Report the source and destination SHAs, patches preserved or backported,
remaining branch work, validation results, and blockers. If publication is not
authorized, leave the local branch ready for review.

When authorized, push the preparation branch and open the PR against the
selected fork destination. The only destination write in this workflow is the
explicitly authorized creation of an absent release branch described above.
For an already published preparation branch that must be
rewritten, verify its head and use an explicit
`--force-with-lease=refs/heads/<preparation-branch>:<observed-sha>`. Never overwrite
another contributor's work. Commit with human sign-off and disclose AI use in
the PR. Describe actual validation, including skips and missing runtime lanes;
use a draft for unresolved integration failures.

Sync and initial-release PRs carrying upstream history must use a merge commit
to preserve that ancestry. Backport PRs may use the repository's usual patch
merge method. Do not merge the PR without authorization. Remove task-owned test
resources while preserving the review branch and useful failure evidence.
