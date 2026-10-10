---
name: sync-upstream-main
description: Merge agent-substrate/substrate upstream/main into the kagent-dev/substrate fork through a pull request, preserving both histories. Use for upstream synchronization of the fork's main branch, not feature-branch rebases or general PR conflict resolution.
---

# Sync Upstream Main

The fork uses merge-based synchronization. Start a branch from current
`origin/main`, merge `upstream/main`, and submit that branch to `origin/main`.
Preserve upstream ancestry so later synchronizations contain only new changes.
Do not replay fork commits, squash upstream history, force-push, or push to main.
Creating a PR requires an explicit request or approval; approval to create it
does not authorize merging it.

## Prepare the merge

1. Confirm remotes (`origin`: `kagent-dev/substrate`; `upstream`:
   `agent-substrate/substrate`), branch, worktree, and local changes. Use an
   isolated worktree under `$TMPDIR` when needed; preserve unrelated work.
2. Fetch both main branches and record their SHAs. Inspect incoming commits and
   existing synchronization PRs before creating duplicate work. If upstream is
   already an ancestor of origin, report that there is nothing to synchronize.
3. Create `sync/upstream-main-YYYYMMDD` from `origin/main` (choose a unique suffix
   if occupied), then run `git merge --no-ff --no-commit upstream/main`.
4. Resolve conflicts using current upstream APIs while retaining intentional
   fork behavior. Inspect automatically merged code where both sides changed
   the same component. Check removed fields and renamed APIs across fork callers.
5. Inspect chart implications of upstream manifest and CRD changes. The fork's
   chart uses agentgateway, while some preserved upstream manifests use Envoy;
   translate applicable behavior without replacing the chart's dataplane. The
   render check excludes preserved manifests, so passing it alone does not
   establish parity. Keep `charts/substrate-crds/templates` synchronized with
   generated CRDs.
6. Commit the merge with `git commit -s`. Keep any additional fixes or workflow
   documentation in focused signed-off commits. Omit issue and PR references
   from commit messages.

## Validate and investigate failures

Run `go test -race ./...`, `hack/verify-all.sh`,
`hack/render-manifests.sh --check`, and `hack/verify/crd-chart.sh`. Use Go's default cache.
Record failures and distinguish regressions from missing prerequisites and
existing failures using evidence from the base revision when needed.

Use `.github/workflows/pr-workflow.yaml` and `.github/workflows/helm-e2e.yaml`
at the merged revision as the source of truth for runtime validation. Follow
the fork's agentgateway configuration and cover both gVisor and microVM when
running E2E. Local E2E must use a task-owned cluster and avoid disrupting shared
clusters or registries. Package tests without `--e2e` are not runtime coverage.

For a failed merge or PR run, identify the exact SHA, job, runtime, and failing
test or setup step. Read test output and component diagnostics together, and
check whether incoming upstream commits already address the failure before
adding another fix. A green unrelated lane or an unexamined rerun is not proof
that the failure is resolved. Report test counts and skips when relevant.

## Publish the PR

Before publishing, fetch both refs again and incorporate any new main changes
with merges. Verify that both recorded main tips are ancestors of the branch,
the diff is intentional, and the worktree is clean. Push only the sync branch.

When PR creation is authorized, open it against `kagent-dev/substrate:main`.
Describe the upstream SHA, material changes, conflict resolutions, required
migration steps, and actual validation. If checks fail or cannot run, disclose
the exact limitation; use a draft for unresolved integration failures. Do not
claim CI or runtime coverage that has not completed on the submitted revision.

State that the PR must be merged with a merge commit to retain upstream
ancestry. Link the PR and report its current checks. Remove task-owned test
resources when finished, preserving the review branch and any useful failure
evidence.
