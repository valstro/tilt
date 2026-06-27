# Tilt fork: build-less attach (image reuse from running cluster)

Goal: a Tilt mode where a workload with `image_deps` **wired** (so
DockerImage/ImageMap/live_update stay associated and rebuild/live_update work on
demand) does **NOT** force an image build before the first `KubernetesApply` on
a fresh `tilt up`. Instead, the first apply reuses the image tag **already
running in the cluster**, and only builds when the user edits a watched file or
clicks rebuild.

This is the one capability stock Tilt cannot do (validated empirically and via
GitHub issue #6649, closed unanswered). It requires a source change.

Repo: github.com/tilt-dev/tilt (Apache-2.0). Local clone: /home/ac/dev/tilt-root
Pinned at v0.37.4 (`06d47a291`).

## Where the forced build happens

`tilt up` → engine → `ImageBuildAndDeployer.BuildAndDeploy`
(`internal/engine/buildcontrol/image_build_and_deployer.go:54`) builds an
`ImageTargetQueue` (`internal/engine/buildcontrol/target_queue.go`).

The queue decides per image target whether to build in
`NewImageTargetQueue` (target_queue.go:61-81):

```go
if state[id].NeedsImageBuild() {
    needsOwnBuild[id] = true
} else if state[id].LastResult != nil {
    // reuse path: only when a PRIOR build exists this session
    ok, _ := canReuseRef(ctx, target, imageRef)  // ib.CanReuseRef
    if !ok { needsOwnBuild[id] = true }
}
```

`BuildState.NeedsImageBuild()` (`internal/store/build_result.go:260`) returns
**true whenever there is no prior image-build result this session** (the
`!lastBuildWasImgBuild` clause). On a fresh `tilt up`/attach there is no
`LastResult`, so every wired image is marked `needsOwnBuild` → built before
deploy. That is the gate.

The existing **reuse** machinery (target_queue.go:66-80, `CanReuseRef` in
`internal/build/image_builder.go:34` → `db.ImageExists`) only triggers when a
`LastResult` already exists. It checks the **local docker image store**, not the
cluster.

## The fork (smallest viable change)

Add an "attach / reuse-from-cluster" path so that, when enabled, an image whose
**cluster-running tag is resolvable** is treated as already-built:

1. **Config plumbing**: a new boolean that reaches `BuildAndDeploy`. Options:
   - simplest: an env var read in `NewImageTargetQueue` /
     `image_build_and_deployer.go` (e.g. `TILT_ATTACH_REUSE_CLUSTER_IMAGES`),
     gated so default behavior is unchanged.
   - cleaner/long-term: a field on `KubernetesApplySpec` or `ImageMapSpec`
     (e.g. `ReuseExistingClusterImage bool`) set by a new Tiltfile arg on
     `k8s_custom_deploy` / `docker_build`. More plumbing (API types in
     `pkg/apis/core/v1alpha1`, codegen, Starlark binding).

2. **Resolve the running tag**: given an image target, read the image currently
   running in the cluster for the workload (the same data Tilt already discovers
   via `KubernetesDiscovery` pods → container images), or accept it via the
   spec/env. Produce a `container.TaggedRefs`.

3. **Short-circuit the queue**: in `NewImageTargetQueue`, when the flag is set
   and a running tag is resolvable AND `state[id].IsEmpty()` (fresh, no prior
   build) AND no files changed:
   - do NOT set `needsOwnBuild[id]`,
   - **backfill a synthetic `ImageBuildResult`** carrying the running ref into
     `q.results[id]` (mirror `backfillExistingResults`, but the result is
     synthesized from the cluster tag rather than a `LastResult`).
   This makes `isBuilding(id)` false → no build stage; `ReusedResults()` lists it
   under "Loading cached images"; the ImageMap is populated from the synthetic
   result; and `deploy()` (line 151) runs with the correct image injected.

4. **Keep wiring intact**: because the ImageTarget/DockerImage/ImageMap/
   LiveUpdate objects are untouched, a later file edit sets
   `FilesChangedSet` → `NeedsImageBuild()` true → normal build+deploy+live_update
   on demand. This is the whole point: build-less first apply, full dev loop
   after.

5. **Pair with the existing fake-deploy helm helper** (already in this repo:
   `tools/tilt/vendor/helm_resource` + `TILT_HELM_ATTACH`): the helm apply
   already skips `helm upgrade` for an installed release. With the queue
   short-circuit, the image side also skips the build. Together =
   true attach: no build, no upgrade, no rollout — yet rebuild/live_update wired.

### Key files to change
- `internal/engine/buildcontrol/target_queue.go` — short-circuit + synthetic
  backfill (core change).
- `internal/build/image_builder.go` — optional: a `CanReuseClusterRef` that
  checks the cluster instead of/in addition to the local store.
- `internal/engine/buildcontrol/image_build_and_deployer.go` — pass the flag and
  the resolved cluster ref into the queue.
- (cluster-image resolution) reuse `internal/k8s` client already wired into the
  IBD, or pass the tag via spec/env to avoid a cluster read in the hot path.
- If going the spec route: `pkg/apis/core/v1alpha1/{kubernetesapply,imagemap}_types.go`
  + `make update-codegen` + Starlark binding in `internal/tiltfile`.

### Build & test the fork
- Build: `make build` (or `go build ./cmd/tilt`); produces a `tilt` binary.
- Targeted tests: `go test ./internal/engine/buildcontrol/...` (there is
  `target_queue`/`build_control_test.go` coverage to extend), plus
  `internal/store` for `NeedsImageBuild`.
- Manual: point OMSKit at the forked binary, `tilt up -- --attach` against
  local-ac, confirm: no build on startup, pods stay on running tag, edit a file
  → live_update, click rebuild → full build+deploy.

### Risk / maintenance
- Localized change (1 core file + plumbing); no architectural change — the
  controller-runtime model isolates it.
- Cost is maintaining a forked binary across releases and distributing it to the
  team/CI. Upstream PR is the lower-maintenance path but #6649 suggests slow
  maintainer engagement.

## Can a cloud agent do this?

Yes — this is a well-scoped Go change in a self-contained package, ideal for an
autonomous coding agent IF it has:
1. The clone (`/home/ac/dev/tilt-root`) and a Go toolchain matching `go.mod`.
2. Ability to run `make build` / `go test ./internal/engine/buildcontrol/...`.
3. A way to validate against a live cluster (the hard part — needs the running
   local-ac env + kube context; an agent sandbox usually lacks this, so the
   cluster-level manual validation would fall back to a human or a recorded
   fixture).

Recommended split:
- **Agent-suitable**: implement the queue short-circuit + synthetic backfill +
  unit tests (steps 1-4 above) entirely against the repo's test suite. This is
  deterministic and verifiable without a cluster.
- **Human/local**: the end-to-end cluster validation (build-less attach +
  live_update) against local-ac, since it needs real k8s + images.

A precise agent task prompt would be: "In /home/ac/dev/tilt-root, add an
env-gated (`TILT_ATTACH_REUSE_CLUSTER_IMAGES`) path in
internal/engine/buildcontrol/target_queue.go so that when set, an image target
with an empty BuildState but a resolvable reuse ref is backfilled with a
synthetic ImageBuildResult instead of being marked needsOwnBuild; thread the
flag and a ref-resolver from BuildAndDeploy; add unit tests in
build_control_test.go covering (a) default unchanged, (b) flag set + ref present
→ no build, (c) flag set + file changed → builds. Run
`go test ./internal/engine/buildcontrol/... ./internal/store/...` and iterate
until green."

## Implementation (DONE)

Implemented against v0.37.4. Env-gated by `TILT_ATTACH_REUSE_CLUSTER_IMAGES`
(truthy = on); default behavior is unchanged.

Files:
- `internal/engine/buildcontrol/target_queue.go` — added
  `ClusterRefResolver`, `NewImageTargetQueueWithClusterReuse`, a `clusterReuse`
  map, the short-circuit in the build-decision loop (fresh/empty state + no
  file/dep change + resolvable running ref → reuse instead of `needsOwnBuild`),
  and synthetic backfill via `store.NewImageBuildResultSingleRef` in
  `backfillExistingResults`.
- `internal/engine/buildcontrol/attach.go` (new) — env gate +
  `clusterRefResolverFor` (matches a running pod container image to the image
  target's cluster repo via `KubernetesDiscovery`, best-effort: returns nil to
  fall back to a build).
- `internal/engine/buildcontrol/image_build_and_deployer.go` — passes the
  resolver into the queue.
- `internal/engine/buildcontrol/target_queue_test.go` — 3 new tests:
  reuse skips build, file-changed still builds (resolver not consulted), no
  running image falls back to build.

Verified: `go test ./internal/engine/buildcontrol/ ./internal/store/` green;
`go vet` clean; `go build ./cmd/tilt` produces a working binary
(`v0.37.4-dev`).

### Using with OMSKit

The engine half (skip build) pairs with the deploy half already in OMSKit
(`tools/tilt/vendor/helm_resource` + `TILT_HELM_ATTACH`, set by `--attach`):

1. Build/install the forked tilt: `make build` (or `go build ./cmd/tilt`), put
   it on PATH ahead of the released tilt.
2. Run OMSKit attach with BOTH halves enabled:
   `TILT_ATTACH_REUSE_CLUSTER_IMAGES=1 tilt up -- --attach --environment local-ac`
   (`--attach` already sets `TILT_HELM_ATTACH=true`).
3. Critically, OMSKit must KEEP image_deps WIRED under `--attach` for this to
   help (the current `--attach` clears them). Change the attach branch in
   `tools/tilt/Tiltfile.extensions` to leave `image_deps`/`image_keys` intact so
   the DockerImage/ImageMap/LiveUpdate stay associated; the fork then prevents
   the startup build by reusing the running image, and a file edit / rebuild
   triggers a normal build + live update.

Result: `tilt up -- --attach` attaches with no image build and no helm upgrade,
yet rebuild + live_update work on demand.

### Manual cluster validation (still TODO — needs live env)
Point OMSKit at the forked binary against a running local-ac and confirm:
- startup: no `accounts` image build, pod keeps its running tilt-<hash> tag;
- edit `apps/entrypoints/accounts/dist/app/...` → live_update sync;
- click rebuild → full build + deploy.
