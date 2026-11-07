// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package install

import (
	"context"
	"fmt"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/ateclient"
	"go.ate.dev/substrate/internal/clustertrustbundle"
)

// trustBundles are published by the certificate controller. Workloads
// project them, so nothing using pod certificates starts before they exist.
var trustBundles = []string{
	"podidentity.podcert.ate.dev:identity:primary-bundle",
	"servicedns.podcert.ate.dev:identity:primary-bundle",
	"postgres.podcert.ate.dev:identity:primary-bundle",
}

func (in *Installer) preflight(ctx context.Context, progress func(string)) (string, error) {
	sv, err := in.kube.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("while reaching the cluster: %w", err)
	}
	if _, err := clustertrustbundle.Discover(in.kube.Discovery()); err != nil {
		return "", fmt.Errorf("ClusterTrustBundle API: %w", err)
	}
	if err := checkPodCertificateAPI(in); err != nil {
		return "", err
	}

	progress("checking for an existing install")
	rel, err := in.helm.get(substrateRelease)
	if err != nil {
		return "", err
	}
	if rel == nil {
		// ate-setup installs with kustomize, which this command can't manage.
		_, err := in.kube.AppsV1().Deployments(SystemNamespace).Get(ctx, "ate-controller", metav1.GetOptions{})
		if err == nil {
			return "", fmt.Errorf("found deployment %s/ate-controller without a substrate Helm release; remove the existing install first", SystemNamespace)
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	return "Kubernetes " + sv.GitVersion, nil
}

// checkPodCertificateAPI checks that the cluster serves PodCertificateRequest,
// which the certificate controller signs.
func checkPodCertificateAPI(in *Installer) error {
	const gv = "certificates.k8s.io/v1beta1"
	res, err := in.kube.Discovery().ServerResourcesForGroupVersion(gv)
	if err == nil {
		for _, r := range res.APIResources {
			if r.Name == "podcertificaterequests" {
				return nil
			}
		}
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("while discovering %s: %w", gv, err)
	}
	return fmt.Errorf("cluster doesn't serve %s podcertificaterequests; enable the PodCertificateRequest feature gate and API", gv)
}

// valuesFunc returns installer defaults, which user values override, and
// forced values, which override user values.
type valuesFunc func(ctx context.Context) (defaults, forced map[string]any, err error)

func noValues(context.Context) (map[string]any, map[string]any, error) { return nil, nil, nil }

// helmStep installs r, or upgrades it if it exists. Install refuses an
// existing release on another chart version, since resuming it would hide a
// real upgrade.
func (in *Installer) helmStep(r release, vf valuesFunc) func(context.Context, func(string)) (string, error) {
	return func(ctx context.Context, progress func(string)) (string, error) {
		ch, err := loadChart(in.charts, r.chart)
		if err != nil {
			return "", err
		}
		cur, err := in.helm.get(r)
		if err != nil {
			return "", err
		}
		if cur != nil && cur.Info.Status == common.StatusPendingInstall && !in.upgrading {
			// An interrupted install leaves this status, and Helm refuses to
			// touch the release until it changes.
			progress(fmt.Sprintf("%s: resuming an interrupted install", r.name))
			if err := in.helm.forget(r, cur.Version); err != nil {
				return "", err
			}
			cur = nil
		}
		if cur != nil {
			if st := cur.Info.Status; st.IsPending() {
				return "", fmt.Errorf("release %s is %s; run helm rollback or helm uninstall on it, then retry", r.name, st)
			}
			if got, want := cur.Chart.Metadata.Version, ch.Metadata.Version; !in.upgrading && got != want {
				return "", fmt.Errorf("release %s has chart version %s, want %s; use kubectl ate upgrade", r.name, got, want)
			}
		}
		vals, err := in.releaseValues(ctx, r, vf, cur == nil)
		if err != nil {
			return "", err
		}
		progress(fmt.Sprintf("%s %s: applying and waiting for workloads", r.name, ch.Metadata.Version))
		if cur == nil {
			rel, err := in.helm.install(ctx, r, ch, vals, in.opts.Timeout.Duration)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s %s installed", r.name, rel.Chart.Metadata.Version), nil
		}
		rel, err := in.helm.upgrade(ctx, r, ch, vals, in.opts.Timeout.Duration)
		if err != nil {
			return "", err
		}
		if from := cur.Chart.Metadata.Version; from != rel.Chart.Metadata.Version {
			return fmt.Sprintf("%s %s → %s, revision %d", r.name, from, rel.Chart.Metadata.Version, rel.Version), nil
		}
		return fmt.Sprintf("%s %s, revision %d", r.name, rel.Chart.Metadata.Version, rel.Version), nil
	}
}

// releaseValues returns the values to install or upgrade r with. Installer
// defaults apply only to a first install. An upgrade reuses the release's
// values, so defaults would override what the user set before.
func (in *Installer) releaseValues(ctx context.Context, r release, vf valuesFunc, first bool) (map[string]any, error) {
	defaults, forced, err := vf(ctx)
	if err != nil {
		return nil, err
	}
	if !first {
		defaults = nil
	}
	user := in.user
	if r == crdsRelease {
		user = nil
	}
	return mergeValues(defaults, user, forced), nil
}

// ateletValues point atelet at the substrate release's RustFS, or at the
// backend of an external store.
func (in *Installer) ateletValues(context.Context) (map[string]any, map[string]any, error) {
	forced := map[string]any{"substrateReleaseName": substrateRelease.name}
	if in.upgrading {
		// The release keeps its storage values.
		return nil, forced, nil
	}
	if in.opts.ObjectStore != ObjectStoreRustFS {
		forced["objectStore"] = map[string]any{"backend": string(in.opts.ObjectStore)}
		return nil, forced, nil
	}
	defaults := map[string]any{
		"objectStore": map[string]any{
			"backend":           "s3",
			"endpoint":          "http://rustfs." + SystemNamespace + ".svc:9000",
			"region":            "us-east-1",
			"pathStyle":         true,
			"credentialsSecret": rustfsCredentialsSecret,
		},
	}
	return defaults, forced, nil
}

func (in *Installer) substrateValues(ctx context.Context) (map[string]any, map[string]any, error) {
	forced := map[string]any{
		// atelet is its own release so it can upgrade first.
		"atelet": map[string]any{"enabled": false},
	}
	if in.upgrading {
		// The release keeps its storage and database values.
		return nil, forced, nil
	}
	if in.opts.ObjectStore == ObjectStoreRustFS {
		access, secret, err := in.rustfsCredentials(ctx)
		if err != nil {
			return nil, nil, err
		}
		forced["rustfs"] = map[string]any{"accessKey": access, "secretKey": secret}
	} else {
		forced["rustfs"] = map[string]any{"enabled": false}
	}
	if in.opts.Postgres == PostgresBundled {
		forced["postgres"] = map[string]any{"clientCertificates": map[string]any{"enabled": true}}
	}
	return nil, forced, nil
}

func (in *Installer) waitTrustBundles(ctx context.Context, progress func(string)) (string, error) {
	ctbs, err := clustertrustbundle.NewClient(in.kube, nil)
	if err != nil {
		return "", err
	}
	var missing []string
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, in.opts.Timeout.Duration, true, func(ctx context.Context) (bool, error) {
		missing = missing[:0]
		for _, name := range trustBundles {
			_, err := ctbs.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				missing = append(missing, name)
				continue
			}
			if err != nil {
				return false, err
			}
		}
		progress(fmt.Sprintf("%d/%d trust bundles published", len(trustBundles)-len(missing), len(trustBundles)))
		return len(missing) == 0, nil
	})
	if err != nil {
		return "", fmt.Errorf("trust bundles %v not published: %w", missing, err)
	}
	return fmt.Sprintf("%d published", len(trustBundles)), nil
}

// checkAPI lists atespaces through the port-forward kubectl ate uses. The
// server can take a moment to accept connections after its pod is ready.
func (in *Installer) checkAPI(ctx context.Context, progress func(string)) (string, error) {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		lastErr = callAPI(ctx, in.opts.Kubeconfig, in.opts.Context)
		if lastErr != nil {
			progress("retrying: " + lastErr.Error())
		}
		return lastErr == nil, nil
	})
	if err != nil {
		return "", fmt.Errorf("ate-api-server not answering: %w", lastErr)
	}
	return "ate-api-server answered", nil
}

func callAPI(ctx context.Context, kubeconfig, kubeContext string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, err := ateclient.NewClient(ctx, kubeconfig, kubeContext, "", "", false)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.ListAtespaces(ctx, &ateapi.ListAtespacesRequest{Options: &ateapi.ListOptions{PageSize: 1}})
	return err
}
