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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader/archive"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/kube"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"go.ate.dev/substrate/internal/version"
)

// release names a Helm release and the chart it installs.
type release struct {
	name      string
	chart     string
	namespace string
}

var (
	crdsRelease      = release{"substrate-crds", "substrate-crds", SystemNamespace}
	podCertRelease   = release{"substrate-podcert", "substrate-podcert", PodCertNamespace}
	ateletRelease    = release{"substrate-atelet", "substrate-atelet", SystemNamespace}
	substrateRelease = release{"substrate", "substrate", SystemNamespace}
)

// allReleases is in install order.
var allReleases = []release{crdsRelease, podCertRelease, ateletRelease, substrateRelease}

type helmClient struct {
	kubeconfig string
	context    string
	log        slog.Handler
}

func newHelmClient(kubeconfig, kubeContext string, log io.Writer) *helmClient {
	if log == nil {
		log = io.Discard
	}
	return &helmClient{
		kubeconfig: kubeconfig,
		context:    kubeContext,
		log:        slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
}

// config returns a Helm configuration that stores releases in namespace.
func (h *helmClient) config(namespace string) (*action.Configuration, error) {
	flags := genericclioptions.NewConfigFlags(true)
	flags.Namespace = &namespace
	if h.kubeconfig != "" {
		flags.KubeConfig = &h.kubeconfig
	}
	if h.context != "" {
		flags.Context = &h.context
	}
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(h.log))
	if err := cfg.Init(flags, namespace, "secret"); err != nil {
		return nil, fmt.Errorf("while initializing Helm: %w", err)
	}
	return cfg, nil
}

// get returns the release, or nil if it doesn't exist.
func (h *helmClient) get(r release) (*releasev1.Release, error) {
	cfg, err := h.config(r.namespace)
	if err != nil {
		return nil, err
	}
	rel, err := action.NewGet(cfg).Run(r.name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("while reading release %s: %w", r.name, err)
	}
	v1, ok := rel.(*releasev1.Release)
	if !ok {
		return nil, fmt.Errorf("release %s has unsupported type %T", r.name, rel)
	}
	return v1, nil
}

func (h *helmClient) install(ctx context.Context, r release, ch *chart.Chart, vals map[string]any, timeout time.Duration) (*releasev1.Release, error) {
	cfg, err := h.config(r.namespace)
	if err != nil {
		return nil, err
	}
	in := action.NewInstall(cfg)
	in.ReleaseName = r.name
	in.Namespace = r.namespace
	in.CreateNamespace = true
	in.WaitStrategy = kube.StatusWatcherStrategy
	in.Timeout = timeout
	return asV1(in.RunWithContext(ctx, ch, vals))
}

// upgrade layers vals over the values r was last installed with, on top of
// the new chart's defaults. Values a user set once persist across upgrades.
func (h *helmClient) upgrade(ctx context.Context, r release, ch *chart.Chart, vals map[string]any, timeout time.Duration) (*releasev1.Release, error) {
	up, err := h.newUpgrade(r)
	if err != nil {
		return nil, err
	}
	up.WaitStrategy = kube.StatusWatcherStrategy
	up.Timeout = timeout
	return asV1(up.RunWithContext(ctx, r.name, ch, vals))
}

// render returns the manifest an upgrade of r would apply, without applying it.
func (h *helmClient) render(ctx context.Context, r release, ch *chart.Chart, vals map[string]any) (string, error) {
	up, err := h.newUpgrade(r)
	if err != nil {
		return "", err
	}
	up.DryRunStrategy = action.DryRunServer
	rel, err := asV1(up.RunWithContext(ctx, r.name, ch, vals))
	if err != nil {
		return "", err
	}
	return rel.Manifest, nil
}

func (h *helmClient) newUpgrade(r release) (*action.Upgrade, error) {
	cfg, err := h.config(r.namespace)
	if err != nil {
		return nil, err
	}
	up := action.NewUpgrade(cfg)
	up.Namespace = r.namespace
	up.ResetThenReuseValues = true
	return up, nil
}

// forget deletes the record of revision version of r and keeps its
// resources. A later install adopts them, since they carry r's ownership
// annotations.
func (h *helmClient) forget(r release, version int) error {
	cfg, err := h.config(r.namespace)
	if err != nil {
		return err
	}
	if _, err := cfg.Releases.Delete(r.name, version); err != nil {
		return fmt.Errorf("while deleting the record of %s revision %d: %w", r.name, version, err)
	}
	return nil
}

func asV1(rel any, err error) (*releasev1.Release, error) {
	if err != nil {
		return nil, err
	}
	v1, ok := rel.(*releasev1.Release)
	if !ok {
		return nil, fmt.Errorf("unsupported release type %T", rel)
	}
	return v1, nil
}

// loadChart reads the chart in directory name of charts.
func loadChart(charts fs.FS, name string) (*chart.Chart, error) {
	var files []*archive.BufferedFile
	err := fs.WalkDir(charts, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(charts, p)
		if err != nil {
			return err
		}
		files = append(files, &archive.BufferedFile{Name: strings.TrimPrefix(p, name+"/"), Data: data})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("while reading chart %s: %w", name, err)
	}
	ch, err := loader.LoadFiles(files)
	if err != nil {
		return nil, fmt.Errorf("while loading chart %s: %w", path.Base(name), err)
	}
	stampVersion(ch, version.Version)
	return ch, nil
}

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// stampVersion sets the chart and app version from a release tag, the same
// way .github/scripts/release/publish-charts.sh does. Dev builds keep the
// versions in Chart.yaml.
func stampVersion(ch *chart.Chart, tag string) {
	if !releaseTag.MatchString(tag) {
		return
	}
	ch.Metadata.Version = strings.TrimPrefix(tag, "v")
	ch.Metadata.AppVersion = tag
}

// mergeValues deep-merges layers. Later layers win. Inputs aren't modified.
func mergeValues(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, l := range layers {
		mergeInto(out, l)
	}
	return out
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			dv, ok := dst[k].(map[string]any)
			if !ok {
				dv = map[string]any{}
				dst[k] = dv
			}
			mergeInto(dv, sv)
			continue
		}
		dst[k] = v
	}
}
