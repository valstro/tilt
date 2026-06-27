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

### Bug fix: live_update never fired after attach (reused ImageMap status not persisted)

Symptom: after `tilt up -- --attach`, an attached resource (e.g. `orders`)
showed `updateStatus: pending` with `runtimeStatus: ok`, no new logs, and
live_update never ran — until a manual full rebuild+redeploy, after which
live_update worked. Inspecting the API: `ImageMap orders` had `image: None`
(empty status) and the `LiveUpdate orders:orders` had `status.containers: null`.

Root cause: the live update reconciler matches running pod containers via
`ImageMap.Status.ImageFromCluster`
(`internal/controllers/apis/liveupdate/liveupdate.go` →
`KubernetesSelectorMatchesContainer`, since OMSKit live updates use an
`ImageMapName` selector). In a normal build, the dockerimage/cmdimage
reconciler's `ForceApply` writes that status to the API server
(`setImageMapStatus` → `client.Status().Update`). In attach reuse mode that
reconciler never runs, so the ImageMap status stayed empty. The fork's merge of
the synthetic result (in `image_build_and_deployer.go`) only updated the
**in-memory `DeepCopy`** used to inject `TILT_IMAGE_i` into the deploy — it was
never written back to the API. With `ImageFromCluster == ""`,
`ImageNamesEqual("", "<running ref>")` is false → zero matched containers →
`LiveUpdate.status.containers: null` → live_update has no target. A full
rebuild "fixed" it only because it ran the real reconciler that persists the
status.

Fix (`internal/engine/buildcontrol/image_build_and_deployer.go`): in the
attach-reuse merge block, after `im.Status = res.ImageMapStatus`, also
`ctrlClient.Status().Update(ctx, im)` so the persisted ImageMap carries the
reused running ref and the live update reconciler can match the container.
(`NewImageBuildResultSingleRef` already populates `ImageFromCluster` correctly.)

Also added a test seam + regression test:
- `ImageBuildAndDeployer.clusterRefResolverForTest` (overrides
  `clusterRefResolverFor`) so the attach path is unit-testable without a live
  cluster/kubectl; production leaves it nil.
- `image_build_and_deployer_test.go::TestAttachReuse_PersistsImageMapStatus`:
  injects a fake running ref, runs `BuildAndDeploy` on empty state, asserts no
  docker build happened AND the **API** ImageMap's `ImageFromCluster` was
  persisted and matches the running container image. The pre-existing
  queue-level reuse tests never caught this because they don't exercise the
  IBD's API writes.

Verified live on local-ac: attach no longer rolls pods, ImageMap populated, and
editing a watched source file triggers live_update with no rebuild/redeploy.

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

## Decision: upstream `initial_sync()` (PR #6737) does NOT replace this fork

Upstream merged `initial_sync()` (tilt-dev/tilt#6737, merged 2026-04-24 as
`0729c7d`; docs follow-up #6758), which addresses issue #2582 ("enable syncing
to a prebuilt image with no docker push"). We evaluated whether it removes the
need for this fork. **It does not.** Keep the fork.

### What `initial_sync()` does

A new `live_update` step (must be first in the step list). On a fresh `tilt up`,
when a container is already running and has never been synced
(`monitor.needsInitialSync()` — no tracked container yet, or
`lastFileTimeSynced.IsZero()`), the live_update reconciler enters `maybeSync`
**with no file-change events** and pushes a **full tar of all `sync()` paths**
into the running container (`reconciler.go` `isInitialSync` branch →
`build.SyncsToPathMappings` → `build.TarArchiveForPaths`). It reuses the same
ignore set as `sync()` (`buildInitialSyncFilter` over the `FileWatch` ignores).

It is purely a **file-sync-into-an-already-running-container** primitive. It does
**nothing** about image builds, the deploy/apply step, or Helm.

### Why it is NOT an attach mechanism

`initial_sync()` assumes Tilt has **already deployed** the workload. It does not
touch the two gates this fork exists to bypass:

1. **The image build gate** (`target_queue.go:64`, `BuildState.NeedsImageBuild()`
   at `internal/store/build_result.go:260`). `initial_sync` does not bypass it.
   The upstream use case sidesteps builds only because it uses a stock pullable
   image (e.g. `nginx:alpine`) via `k8s_custom_deploy` with **no Tilt-managed
   build target** — so there was never a build to gate.
2. **The Helm upgrade / apply** (our `helm_resource` → `helm upgrade`, which
   rolls pods). `initial_sync` never prevents Tilt from re-applying manifests.

The two solve **different halves**:

| Concern                              | This fork (`--attach`)                       | `initial_sync()` (#6737)            |
|--------------------------------------|----------------------------------------------|-------------------------------------|
| Skip image build on startup          | yes (synthetic `ImageBuildResult` reuse)     | no (only if you have no build target) |
| Skip Helm upgrade / no pod roll      | yes (`TILT_HELM_ATTACH` helm-helper patch)   | no (not addressed)                  |
| Attach to live pods & logs           | yes                                          | n/a (assumes already deployed)      |
| Working live_update after attach     | yes                                          | yes (the part it does well)         |
| Seed container w/ local source on start | no (relies on the running image being right) | yes (its whole point)            |

Replacing the fork with stock Tilt + `initial_sync` would require rewriting
OMSKit to have **no image-build target and no Helm apply** for attached services
(e.g. `k8s_custom_deploy` with a no-op apply) — far more invasive, and
`k8s_custom_deploy` still runs its `apply_cmd`. The supported deploy paths
(`docker_build`/`k8s_yaml`/`helm`) all still gate the first apply on a built
ImageMap (the wall this fork was built for; cf. issue #6649).

### Where it IS useful to us (adopt additively)

`initial_sync()` is the upstream-blessed fix for one real limitation of the
current attach: today we **assume the running cluster image already contains the
right code**, and live_update only pushes edits made *after* attaching. If the
local tree has drifted from what's baked in the attached pod, attach does not
reconcile that drift on connect.

Decision:
- **Keep the fork.** `initial_sync()` removes neither the build-skip nor the
  helm-upgrade-skip; it is not an attach mechanism.
- **Adopt `initial_sync()` additively** after rebasing the fork onto a `master`
  that includes `0729c7d`: add `initial_sync()` as the first live_update step for
  attached services so attach reconciles local↔pod drift on connect. Strictly an
  improvement to the existing attach UX, not a replacement.
- **Strategic upside:** pulling `0729c7d` into the fork is low-cost, reduces
  long-term divergence, and strengthens the eventual upstream-PR story — our
  `TILT_ATTACH_REUSE_CLUSTER_IMAGES` build-skip is the natural complement to the
  now-merged `initial_sync` sync-skip.

Open follow-ups (not yet done): (1) confirm whether the fork's v0.37.4 base
already contains `0729c7d`, scope a rebase if not; (2) prototype `initial_sync()`
in the OMSKit live_update step lists under `--attach` and validate drift
reconcile on local-ac.

## Building & releasing the binary

The fork is distributed as a prebuilt binary on GitHub Releases
(https://github.com/valstro/tilt/releases). Consumers install it as
`tilt-attach` (see omskit `tools/tilt/README.md`).

### Build a release binary (linux/amd64)

From the fork checkout, at the commit you want to release:

```bash
cd /path/to/tilt-fork
git checkout <ref>                 # the fork commit/tag to release
SHA=$(git rev-parse --short HEAD)

# stripped (-s -w) to shrink the binary; embeds the fork SHA in the version.
go build -mod vendor \
  -ldflags "-s -w -X 'github.com/tilt-dev/tilt/internal/cli.commitSHA=${SHA}-attachfork'" \
  -o /tmp/tilt-attach ./cmd/tilt

/tmp/tilt-attach version   # -> v0.37.4-dev
```

Notes:
- `devVersion` (`internal/cli/build.go`) is the upstream base (currently
  `0.37.4`); `-dev` is appended automatically for non-tagged builds.
- `-s -w` strips debug symbols (≈170MB → ≈121MB on disk). Drop them if you want
  a debuggable binary.
- This produces a **linux/amd64** binary (build host arch). For other platforms
  set `GOOS`/`GOARCH`, e.g. `GOOS=darwin GOARCH=arm64 go build ...`.

### Package the release asset

```bash
SHA=$(git rev-parse --short HEAD)
NAME="tilt-attach-v0.37.4-${SHA}-linux-amd64"

mkdir -p "$NAME"
cp /tmp/tilt-attach "$NAME/tilt-attach" && chmod +x "$NAME/tilt-attach"
# (optional) add a README.txt with install/usage notes
tar -czf "${NAME}.tar.gz" "$NAME"
# NOTE: publish the checksum as `.sha256.txt`, NOT a bare `.sha256`. The GitHub
# release uploads host (uploads.github.com) returns 403 for a `.sha256`
# extension asset; `.sha256.txt` uploads fine. The tarball itself uploads OK.
sha256sum "${NAME}.tar.gz" | tee "${NAME}.tar.gz.sha256.txt"   # filename-relative
rm -rf "$NAME"
```

The `.sha256.txt` is filename-relative so consumers can `sha256sum -c` from
their download dir.

### Publish

Create the release first, then upload assets separately (the combined
create+upload occasionally 403s mid-upload; splitting is more robust). Tag with
the upstream version + fork SHA, e.g. `0.37.5-<sha>`:

```bash
gh release create "0.37.5-${SHA}" \
  --repo valstro/tilt \
  --target feat/attach \
  --title "tilt-attach 0.37.4 (${SHA})" \
  --notes "attach-fork tilt: image reuse + port-forward pod-pick fix + reused-ImageMap-status persistence. See FORK_ATTACH_DESIGN.md."

gh release upload "0.37.5-${SHA}" --repo valstro/tilt "${NAME}.tar.gz"
gh release upload "0.37.5-${SHA}" --repo valstro/tilt "${NAME}.tar.gz.sha256.txt"
```

Retire the previous release/tag so there is a single current download:

```bash
gh release delete "0.37.5-<oldSHA>" --repo valstro/tilt --cleanup-tag --yes
```

Then update the `TAG`/`ASSET` (and the `.sha256.txt` pattern) in omskit
`tools/tilt/README.md` to match.
