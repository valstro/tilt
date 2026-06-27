package buildcontrol

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/distribution/reference"

	"github.com/tilt-dev/tilt/internal/container"
	"github.com/tilt-dev/tilt/pkg/logger"
	"github.com/tilt-dev/tilt/pkg/model"
)

// AttachReuseClusterImagesEnv, when set to a truthy value, enables "attach"
// mode: on a fresh `tilt up`, an image target that has never been built this
// session reuses the image tag already running in the cluster instead of
// forcing a build. The DockerImage/ImageMap/LiveUpdate wiring is left intact,
// so editing a watched file (or clicking rebuild) still triggers a normal
// build + deploy + live update.
//
// This is the engine half of true attach mode; pair it with a deploy path that
// doesn't redeploy (e.g. OMSKit's vendored helm_resource TILT_HELM_ATTACH).
//
// It can also be enabled implicitly by passing `--attach` as a Tiltfile arg
// (`tilt up -- --attach`), so no env var is needed in normal use. The env var
// remains useful for tests and for Tiltfiles that don't define an --attach arg.
const AttachReuseClusterImagesEnv = "TILT_ATTACH_REUSE_CLUSTER_IMAGES"

// AttachTiltfileArg is the Tiltfile arg (after `--`) that implicitly enables
// attach image reuse.
const AttachTiltfileArg = "--attach"

func attachReuseClusterImagesEnabled(tiltfileArgs []string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(AttachReuseClusterImagesEnv)))
	if v == "1" || v == "true" || v == "yes" {
		return true
	}
	for _, a := range tiltfileArgs {
		// Match `--attach` and `--attach=...` / `--attach true`.
		if a == AttachTiltfileArg || strings.HasPrefix(a, AttachTiltfileArg+"=") {
			return true
		}
	}
	return false
}

// clusterRefResolverFor builds a ClusterRefResolver that finds the image
// currently running in the cluster for an image target. It queries the cluster
// DIRECTLY via kubectl (not Tilt's KubernetesDiscovery object), so it works on
// a fresh `tilt up` before discovery has populated — avoiding the race where
// the build decision runs before the KubernetesDiscovery exists.
//
// Returns nil when attach mode is disabled (queue uses normal build behavior).
func (ibd *ImageBuildAndDeployer) clusterRefResolverFor(kTarget model.K8sTarget, tiltfileArgs []string) ClusterRefResolver {
	if !attachReuseClusterImagesEnabled(tiltfileArgs) {
		return nil
	}

	namespace := namespaceFromApplyCmd(kTarget)

	return func(ctx context.Context, iTarget model.ImageTarget) (reference.NamedTagged, error) {
		wantRepo := iTarget.ImageMapSpec.Selector
		if refs, err := iTarget.Refs(nil); err == nil {
			wantRepo = reference.FamiliarName(refs.ClusterRef())
		}

		images := runningClusterImagesViaKubectl(ctx, namespace)

		for _, img := range images {
			named, err := container.ParseNamedTagged(img)
			if err != nil {
				continue
			}
			if repoMatches(reference.FamiliarName(named), wantRepo) {
				logger.Get(ctx).Infof("attach: reusing running image %s for %s (skipping build)", img, iTarget.ID().Name)
				return named, nil
			}
		}
		logger.Get(ctx).Infof("attach: no running image matched %q for %s; building", wantRepo, iTarget.ID().Name)
		return nil, nil
	}
}

// repoMatches reports whether a running image's repository corresponds to the
// target's wanted repository. Because Refs(nil) yields the configuration ref
// WITHOUT the cluster's registry prefix (we don't have the Cluster object
// here), we match on path suffix: a running `<host>/valstro-dev/ac/orders`
// matches a wanted `orders` or `valstro-dev/ac/orders`, and vice versa.
func repoMatches(runningRepo, wantRepo string) bool {
	if runningRepo == wantRepo {
		return true
	}
	rTrim := strings.TrimSuffix(runningRepo, "/")
	wTrim := strings.TrimSuffix(wantRepo, "/")
	// One repo is the registry/namespace-prefixed form of the other.
	return strings.HasSuffix(rTrim, "/"+wTrim) || strings.HasSuffix(wTrim, "/"+rTrim)
}

// namespaceFromApplyCmd extracts NAMESPACE=... from the k8s target's apply cmd
// env (set by the helm_resource extension). Empty if not present.
func namespaceFromApplyCmd(kTarget model.K8sTarget) string {
	spec := kTarget.KubernetesApplySpec
	if spec.ApplyCmd == nil {
		return ""
	}
	for _, e := range spec.ApplyCmd.Env {
		if strings.HasPrefix(e, "NAMESPACE=") {
			return strings.TrimPrefix(e, "NAMESPACE=")
		}
	}
	return ""
}

// runningClusterImagesViaKubectl lists container images of running pods in the
// namespace by shelling out to kubectl. Best-effort: returns nil on any error
// (the caller then falls back to a normal build).
func runningClusterImagesViaKubectl(ctx context.Context, namespace string) []string {
	args := []string{"get", "pods",
		"-o", "jsonpath={range .items[*]}{range .spec.containers[*]}{.image}{\"\\n\"}{end}{end}"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		logger.Get(ctx).Infof("attach: kubectl get pods failed (ns=%q): %v", namespace, err)
		return nil
	}
	var images []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			images = append(images, line)
		}
	}
	return images
}
