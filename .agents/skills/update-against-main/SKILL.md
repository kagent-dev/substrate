---
name: update-against-main
description: Merge agent-substrate/substrate main into the kagent-dev/substrate fork's main branch, resolve conflicts, validate the result, and safely update the fork. Use only when explicitly synchronizing the fork's main branch with upstream main. Do not use for updating, rebasing, or resolving conflicts in feature branches or pull requests.
---

# Update Against Main

This skill applies only to synchronizing the fork's `main` branch. Do not invoke it for a feature branch or PR merely because that branch is behind or conflicts with `main`.

1. Confirm the worktree, current branch, tracking branch, and remotes. Do not disturb unrelated changes.
2. Fetch `origin/main` and `upstream/main`, inspect their divergence, and create a dated backup branch from `origin/main`.
3. Rebuild `main` from `upstream/main` by replaying only intentional fork feature commits in dependency order. Drop merge commits and fork commits superseded by upstream.
4. Resolve conflicts in favor of current upstream APIs while preserving the remaining fork features. Inspect the resulting diff and linear history.
5. Keep Helm charts synchronized with their corresponding manifests. When either changes, inspect and update the other while preserving intentional Helm templating and conditionals, then run `make verify-helm-template` and `make verify-crd-chart` and compare any relevant resources not covered by those checks.
6. Run `make test` and `make verify`.
7. Run the real Kind E2E matrix from `.github/workflows/pr-workflow.yaml`, but use agentgateway for all fork testing:
   - Recreate the cluster with `hack/create-kind-cluster.sh`.
   - Install the control plane with `hack/install-ate-kind.sh --deploy-ate-system --atenet-router=agentgateway`.
   - Deploy the micro-VM demo with `hack/run-microvm-demo-kind.sh --skip-control-plane` so it does not reinstall the control plane.
   - Deploy the gVisor counter demo and both standard egress demos.
   - The full gVisor suite: `hack/run-e2e-kind.sh -v -args --no-color`
   - The full micro-VM suite with the CI environment: `E2E_SANDBOX_CLASS=microvm hack/run-e2e-kind.sh -v -args --no-color`
   - Switch egress to agentgateway sdsmint, then run the MITM trust and targeted networking lanes for both runtimes exactly as the workflow specifies.
   - Verify the live router and egress workloads use agentgateway. Never use Envoy for fork validation.
8. Treat `go test ./internal/e2e/...` without `-args --e2e` as compilation/package testing, not E2E coverage.
9. Do not push when unit, verification, or E2E checks fail or cannot run. Report the exact blocker instead.
10. After all checks pass, verify the worktree and rewritten commits, then update the fork with `git push --force-with-lease origin main`. Never use an unguarded force push.

Use the current CI workflow as the source of truth for cluster setup, images, demos, runtime coverage, and environment variables, with the agentgateway-only override above. Never claim E2E passed unless workloads ran against the cluster.
