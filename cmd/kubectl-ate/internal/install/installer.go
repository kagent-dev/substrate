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

// Package install installs Agent Substrate from its Helm charts.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"helm.sh/helm/v4/pkg/cli/values"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	chartsfs "go.ate.dev/substrate/charts"
	"go.ate.dev/substrate/internal/ateclient"
	"go.ate.dev/substrate/internal/installdefaults"
)

const (
	// SystemNamespace holds the control plane, atelet and PostgreSQL.
	SystemNamespace = installdefaults.SystemNamespace
	// PodCertNamespace holds the certificate controller and its CA pools.
	PodCertNamespace = "podcertificate-controller-system"
)

// PostgresMode selects where the control plane's database comes from.
type PostgresMode string

const (
	// PostgresBundled applies manifests/ate-install/postgres for evaluation.
	// It has no backups or high availability.
	PostgresBundled PostgresMode = "bundled"
	// PostgresExternal uses connection Secrets the user created.
	PostgresExternal PostgresMode = "external"
)

// ObjectStore selects where snapshots are stored.
type ObjectStore string

const (
	// ObjectStoreRustFS runs RustFS in the cluster with the substrate release.
	ObjectStoreRustFS ObjectStore = "rustfs"
	// ObjectStoreS3 and ObjectStoreGCS use a bucket the user configures with
	// the charts' objectStore, objectStorePlugin and ateApi values.
	ObjectStoreS3  ObjectStore = "s3"
	ObjectStoreGCS ObjectStore = "gcs"
)

// Options configures an Installer.
type Options struct {
	Kubeconfig string
	Context    string
	// ChartDir loads charts from a directory instead of the embedded copies.
	ChartDir string
	// ValuesFiles and Values follow helm's -f and --set. They apply to every
	// release except substrate-crds.
	ValuesFiles []string
	Values      []string
	Postgres    PostgresMode
	ObjectStore ObjectStore
	// MicroVM stages the microVM guest assets and applies the microvm
	// SandboxConfig.
	MicroVM bool
	// MicroVMAssetBucket is the bucket for microVM assets with an s3 or gcs
	// object store. RustFS uses its snapshot bucket.
	MicroVMAssetBucket string
	// Timeout bounds each wait for workloads to become ready.
	Timeout time.Duration
	// HelmLog receives Helm's debug log. Nil discards it.
	HelmLog io.Writer
}

// Installer runs install steps against one cluster.
type Installer struct {
	opts   Options
	rest   *rest.Config
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	helm   *helmClient
	charts fs.FS
	report Reporter
	// user holds the merged ValuesFiles and Values.
	user map[string]any
}

// New connects to the cluster that opts select.
func New(opts Options, r Reporter) (*Installer, error) {
	restCfg, err := ateclient.LoadKubeConfig(opts.Kubeconfig, opts.Context)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("while creating Kubernetes client: %w", err)
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("while creating Kubernetes client: %w", err)
	}
	charts := fs.FS(chartsfs.FS)
	if opts.ChartDir != "" {
		charts = os.DirFS(opts.ChartDir)
	}
	valueOpts := values.Options{ValueFiles: opts.ValuesFiles, Values: opts.Values}
	user, err := valueOpts.MergeValues(nil)
	if err != nil {
		return nil, fmt.Errorf("while reading values: %w", err)
	}
	return &Installer{
		opts:   opts,
		rest:   restCfg,
		kube:   kc,
		dyn:    dc,
		helm:   newHelmClient(opts.Kubeconfig, opts.Context, opts.HelmLog),
		charts: charts,
		report: r,
		user:   user,
	}, nil
}

// step is one unit of work. run returns a short summary for the report.
type step struct {
	name string
	run  func(ctx context.Context, progress func(string)) (string, error)
}

func (in *Installer) run(ctx context.Context, steps []step) error {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.name
	}
	in.report.Report(Event{Kind: Plan, Steps: names})
	for _, s := range steps {
		in.report.Report(Event{Kind: StepStarted, Step: s.name})
		start := time.Now()
		progress := func(msg string) {
			in.report.Report(Event{Kind: StepProgress, Step: s.name, Msg: msg})
		}
		msg, err := s.run(ctx, progress)
		elapsed := time.Since(start)
		if err != nil {
			in.report.Report(Event{Kind: StepFailed, Step: s.name, Err: err, Elapsed: elapsed})
			return fmt.Errorf("%s: %w", s.name, err)
		}
		in.report.Report(Event{Kind: StepDone, Step: s.name, Msg: msg, Elapsed: elapsed})
	}
	return nil
}

// Install installs every release in order. Rerunning it is safe and resumes a
// failed install: existing secrets are kept and existing releases are
// upgraded in place when their chart version matches.
func (in *Installer) Install(ctx context.Context) error {
	switch in.opts.Postgres {
	case PostgresBundled, PostgresExternal:
	default:
		return fmt.Errorf("invalid postgres mode %q: must be %s or %s", in.opts.Postgres, PostgresBundled, PostgresExternal)
	}
	switch in.opts.ObjectStore {
	case ObjectStoreRustFS, ObjectStoreS3, ObjectStoreGCS:
	default:
		return fmt.Errorf("invalid object store %q: must be %s, %s or %s", in.opts.ObjectStore, ObjectStoreRustFS, ObjectStoreS3, ObjectStoreGCS)
	}
	if in.opts.MicroVM && in.opts.ObjectStore != ObjectStoreRustFS && in.opts.MicroVMAssetBucket == "" {
		return errors.New("--microvm with an s3 or gcs object store needs --microvm-asset-bucket")
	}
	if in.opts.Postgres == PostgresBundled {
		in.report.Report(Event{Kind: Note, Msg: "Installing a single-replica PostgreSQL for evaluation only. It has no backups or high availability. Use --postgres external with a managed database for anything else."})
	}
	steps := []step{
		{"Check cluster", in.preflight},
		{"Install CRDs", in.helmStep(crdsRelease, noValues)},
		{"Create pod certificate CAs", in.createPodCertCAs},
		{"Install certificate controller", in.helmStep(podCertRelease, noValues)},
		{"Wait for trust bundles", in.waitTrustBundles},
		{"Create identity secrets", in.createIdentitySecrets},
		{"Configure object storage", in.configureObjectStore},
		{"Install atelet", in.helmStep(ateletRelease, in.ateletValues)},
	}
	if in.opts.Postgres == PostgresBundled {
		steps = append(steps,
			step{"Install PostgreSQL", in.applyPostgres},
			step{"Set up database", in.setupDatabase},
		)
	} else {
		steps = append(steps, step{"Check database secrets", in.checkDatabaseSecrets})
	}
	steps = append(steps, step{"Install Substrate", in.helmStep(substrateRelease, in.substrateValues)})
	if in.opts.MicroVM {
		steps = append(steps,
			step{"Stage microVM assets", in.stageMicroVMAssets},
			step{"Apply microVM SandboxConfig", in.applyMicroVMSandboxConfig},
		)
	}
	steps = append(steps, step{"Check API", in.checkAPI})
	return in.run(ctx, steps)
}
