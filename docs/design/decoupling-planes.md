# Decoupling the Substrate Planes

Status: working draft

## Problem

Substrate currently exposes two API surfaces:

- Kubernetes CRDs for declarative configuration: `ActorTemplate`, `WorkerPool`,
  and `SandboxConfig`.
- `ate-api-server` gRPC for dynamic state: `Atespace`, `Actor`, and `Worker`.

The target design is to remove the current Substrate CRDs as the user-facing
API. `ate-api-server` should become the source of truth for Substrate resources,
with Kubernetes kept as an implementation backend for running pods,
deployments, certificates, services, and other cluster machinery.

Logs are intentionally out of scope for this draft.

## Goals

- Make all user-facing Substrate resources available through CLI/API backed by
  `ate-api-server`.
- Remove Kubernetes object identity from user-facing runtime calls.
- Keep Kubernetes available as the infrastructure backend, especially for
  self-hosted deployments.
- Keep `atecontroller` as a projector from Substrate state to Kubernetes
  resources, instead of moving Kubernetes apply logic into `ate-api-server`.
- Preserve the current high-scale path where `Actor` and `Worker` state live
  outside Kubernetes.

## Non-Goals

- Designing GitOps/YAML workflows. Start with CLI/API first.
- Reworking actor log access.
- Solving the full authorization, quota, and policy model in this draft.
- Renaming every resource concept. Names can stay boring until a stronger model
  proves itself.

## Target Plane Split

```text
User/API plane
  ate-api-server gRPC
  CLI
  Resource CRUD and lifecycle operations

Runtime/control plane
  ate-api-server workflows
  Redis/Valkey-backed resource store
  atelet
  atenet
  snapshot storage

Infrastructure plane
  Kubernetes Deployments, Pods, Services, Secrets, certificates
  atecontroller as projector
```

## Target Resource Model

`Atespace` is the user-facing tenancy boundary.

```text
Atespace
  ActorTemplate
  Actor

Cluster/admin scope, exact model TBD
  WorkerPool
  SandboxConfig
```

### Atespace

An `Atespace` groups actors and actor templates. It should continue to be a
Substrate-native resource stored by `ate-api-server`.

### ActorTemplate

`ActorTemplate` should disappear as a Kubernetes CRD, but remain as a gRPC
resource.

Actor templates are scoped to an atespace:

```text
actor_template:<atespace>:<name>
```

Open decisions:

- Whether template specs remain immutable, as they are today.
- Whether deleting a template is allowed while actors still reference it.
- Whether template versioning should be explicit now or deferred.

### Actor

`Actor` remains a high-frequency runtime record owned by `ate-api-server`.

`CreateActor` should stop accepting Kubernetes namespace/name references. It
should reference an atespace-scoped actor template instead.

Example shape:

```proto
message ActorTemplateRef {
  string atespace = 1;
  string name = 2;
}

message CreateActorRequest {
  ActorRef actor_ref = 1;
  ActorTemplateRef actor_template = 2;
  Selector worker_selector = 3;
}
```

### WorkerPool

`WorkerPool` should disappear as a Kubernetes CRD, but remain available through
the API for self-hosted and admin use. SaaS deployments can hide it from normal
users.

The scoping model is the main open design fork.

#### Option A: Global WorkerPools

Worker pools are cluster/admin resources. Actor templates select pools by
class, labels, and policy.

Pros:

- Closest to the current implementation.
- Simple scheduling model.
- Easy to hide from SaaS users.

Cons:

- Tenant isolation must be enforced through authorization, selectors, and
  admission checks.

#### Option B: Atespace-Scoped WorkerPools

Each atespace owns its own worker pools.

Pros:

- Clear ownership and quota boundary.
- Easy to explain to users.

Cons:

- Fragments capacity.
- Makes shared infrastructure awkward.
- Poor fit for SaaS systems with centrally managed workers.

#### Option C: Global WorkerPools With Atespace Grants

Worker pools are admin-owned resources, and an atespace can only schedule onto
pools granted to it.

This is the selected direction.

Pros:

- Preserves shared infrastructure.
- Gives the API a real tenancy boundary.
- Lets SaaS hide pools while self-hosted users can manage them directly.

Cons:

- Requires a grant/policy resource.
- Still needs quota and authorization work later.

Example shape:

```proto
message WorkerPool {
  string name = 1;
  int64 version = 2;
  map<string, string> labels = 3;
  WorkerPoolSpec spec = 4;
  WorkerPoolStatus status = 5;
}

message WorkerPoolGrant {
  string atespace = 1;
  string worker_pool = 2;
}
```

### SandboxConfig

`SandboxConfig` should disappear as a Kubernetes CRD, but remain an admin API
resource.

Open decisions:

- Whether `SandboxConfig` is completely admin-only.
- Whether users can discover sandbox classes.
- Whether sandbox class requirements should define Kubernetes pod shape instead
  of hardcoding class-specific logic in the projector.

### Secrets

The current `ActorTemplate` API supports Kubernetes `SecretKeyRef` in env vars.
That cannot remain the user-facing model if CRDs disappear.

Minimal direction:

- Add a Substrate-native secret reference to the gRPC API.
- Let self-hosted implementations project or resolve those references through
  Kubernetes `Secret`s internally.

Detailed secret API design is deferred.

## Projector Model

`atecontroller` should become a projector from the `ate-api-server` resource
store to Kubernetes.

```text
WorkerPool in ate-api-server store
  -> atecontroller
  -> Kubernetes Deployment
  -> worker Pods
  -> ate-api-server Worker records
```

The golden snapshot lifecycle should also move off CRD status updates. The
controller/workflow should update `ActorTemplate.status` in the
`ate-api-server` store.

```text
CreateActorTemplate
  -> store template as Pending
  -> controller creates/resumes golden actor
  -> controller suspends golden actor
  -> store template as Ready with golden snapshot URI
```

## Migration Plan

1. Add gRPC/store resources for `ActorTemplate`, `WorkerPool`, and
   `SandboxConfig`.
2. Change `ate-api-server` workflows to read these resources from the store
   instead of Kubernetes listers.
3. Change `CreateActor` to reference an atespace-scoped `ActorTemplate`.
4. Convert `atecontroller` from a CRD reconciler into a store-backed projector
   for `WorkerPool` and golden snapshot workflows.
5. Keep the Kubernetes worker pod syncer: worker pods are still Kubernetes
   objects and should still be mirrored into `Worker` records.
6. Add CLI commands for create/get/list/delete of the new API resources.
7. Remove CRDs after the CLI/API path covers the current resource lifecycle.

## Implementation Work Items

### API and Generated Types

- Add `ActorTemplate`, `ActorTemplateSpec`, `ActorTemplateStatus`,
  `ActorTemplateRef`, `WorkerPool`, `WorkerPoolSpec`, `WorkerPoolStatus`,
  `SandboxConfig`, and related nested messages to `ateapi.proto`.
- Add CRUD RPCs for `ActorTemplate`, `WorkerPool`, and `SandboxConfig`.
- Change `CreateActorRequest` to reference an `ActorTemplateRef` instead of
  `actor_template_namespace` and `actor_template_name`.
- Regenerate protobuf Go code.
- Decide whether to keep compatibility fields temporarily or make a breaking
  proto change while APIs are still unstable.

### Validation

- Move CRD validation rules into Go validation used by gRPC handlers.
- Preserve current checks for image pinning, DNS names, env vars, readiness
  probes, snapshot config, volume mounts, sandbox class, and worker selectors.
- Add validation for atespace-scoped template identity.
- Add validation for `WorkerPool` scope once the scope/grant model is chosen.

### Store

- Extend the persistence interface with CRUD/list methods for `ActorTemplate`,
  `WorkerPool`, and `SandboxConfig`.
- Add Redis key layouts for the new resources.
- Add optimistic version handling for mutable resources.
- Add list/pagination behavior where needed.
- Add store tests for create/get/update/delete/list and conflict behavior.

### Control API

- Implement gRPC handlers for the new resources.
- Update `CreateActor` to load the template from the store.
- Update resume, pause, and suspend workflows to load `ActorTemplate`,
  `WorkerPool`, and `SandboxConfig` from the store instead of Kubernetes
  listers.
- Replace Kubernetes `ActorTemplate` status reads with store-backed
  `ActorTemplate.status`.
- Keep Kubernetes pod and secret access only where it is still explicitly an
  infrastructure concern.

### Golden Snapshot Workflow

- Move the current `ActorTemplate` CRD reconcile state machine to a
  store-backed workflow.
- On `CreateActorTemplate`, store the template in a pending phase.
- Create/resume/suspend the golden actor through existing actor lifecycle RPC
  logic.
- Update `ActorTemplate.status` with phase, golden actor ID, and golden
  snapshot URI.
- Define retry behavior and failure state for failed golden snapshot creation.

### WorkerPool Projector

- Convert `atecontroller` from CRD watches to a store-backed projector.
- Watch or poll `WorkerPool` resources from `ate-api-server`.
- Reuse the existing Deployment apply builder where possible.
- Update `WorkerPool.status` in the store from observed Kubernetes Deployment
  state.
- Keep the existing worker pod syncer that mirrors Kubernetes pods into
  `Worker` records.

### Scheduler and Grants

- Implement the chosen `WorkerPool` scope model.
- If using global pools with grants, add `WorkerPoolGrant` API/store support.
- Filter eligible pools by sandbox class, labels, actor selector, template
  selector, and grant policy.
- Add tests for cross-atespace scheduling denial.

### CLI

- Add `kubectl ate create/get/delete actor-template`.
- Add admin-facing commands for `workerpool` and `sandboxconfig`.
- Update `create actor` to accept an atespace-scoped template reference.
- Remove or hide Kubernetes namespace/template naming from user-facing
  commands.
- Keep direct YAML/GitOps support out of the first CLI pass.

### Manifests and RBAC

- Remove CRD installation from the default install path once replacement APIs
  exist.
- Remove `ate-api-server` RBAC for Substrate CRDs.
- Keep Kubernetes RBAC needed for pods, deployments, services, certificates,
  and any internal secret projection.
- Update generated manifests and install scripts.

### Tests

- Add unit tests for gRPC validation and store behavior.
- Update `ateapi` functional tests to create templates and pools through the
  new API instead of Kubernetes CRDs.
- Update controller tests to drive store-backed `WorkerPool` projection.
- Keep existing worker lifecycle tests, but remove CRD setup helpers.
- Add migration or compatibility tests only if temporary compatibility fields
  are kept.

### Cleanup

- Delete CRD Go types, generated clients, listers, informers, and manifests
  after all code paths stop importing them.
- Delete CRD validation tests after equivalent gRPC validation tests exist.
- Update docs, examples, demos, and quickstarts to use CLI/API resources.
- Remove old Kubernetes namespace/template references from metrics and output
  where they no longer match the API model.

## Open Questions

- Should `WorkerPool` be global, atespace-scoped, or global with grants?
- Are `ActorTemplate` specs immutable?
- Can an `ActorTemplate` be deleted while actors still reference it?
- Is `WorkerPoolGrant` sufficient for the first version, or do quotas need to
  exist at the same time?
- Are `SandboxConfig`s completely admin-only?
- What exactly replaces Kubernetes `SecretKeyRef` in `ActorTemplate.env`?
- Do we need API watch/streaming semantics for resource status, or is polling
  enough initially?
