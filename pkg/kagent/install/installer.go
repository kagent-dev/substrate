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
	"fmt"
	"io"
	"io/fs"
	"time"

	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
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

// PostgresType selects where the control plane's database comes from.
type PostgresType string

const (
	// PostgresBundled applies manifests/ate-install/postgres for evaluation.
	// It has no backups or high availability.
	PostgresBundled PostgresType = "bundled"
	// PostgresExternal uses connection Secrets the user created.
	PostgresExternal PostgresType = "external"
)

// ObjectStoreType selects where snapshots are stored.
type ObjectStoreType string

const (
	// ObjectStoreBundled runs RustFS in the cluster with the substrate
	// release, for evaluation.
	ObjectStoreBundled ObjectStoreType = "bundled"
	// ObjectStoreS3 and ObjectStoreGCS use a bucket the user created.
	ObjectStoreS3  ObjectStoreType = "s3"
	ObjectStoreGCS ObjectStoreType = "gcs"
)

// Options configures an Installer.
type Options struct {
	Config
	Kubeconfig string
	Context    string
	// Charts holds a directory per chart. Nil uses the embedded charts.
	Charts fs.FS
	// HelmLog receives Helm's debug log. Nil discards it.
	HelmLog io.Writer
}

// Installer runs install steps against one cluster.
type Installer struct {
	opts   Options
	rest   *rest.Config
	kube   kubernetes.Interface
	apiext apiextclient.Interface
	dyn    dynamic.Interface
	helm   *helmClient
	charts fs.FS
	report Reporter
	// user holds each release's values from Config.Releases, by release name.
	user map[string]map[string]any
	// upgrading lets releases move to a new chart version.
	upgrading bool
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
	ac, err := apiextclient.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("while creating Kubernetes client: %w", err)
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("while creating Kubernetes client: %w", err)
	}
	charts := opts.Charts
	if charts == nil {
		charts = chartsfs.FS
	}
	user, err := opts.userValues()
	if err != nil {
		return nil, err
	}
	return &Installer{
		opts:   opts,
		rest:   restCfg,
		kube:   kc,
		apiext: ac,
		dyn:    dc,
		helm:   newHelmClient(opts.Kubeconfig, opts.Context, opts.HelmLog),
		charts: charts,
		report: r,
		user:   user,
	}, nil
}

// Step is one unit of work. Run returns a short summary for the report.
type Step struct {
	Name string
	Run  func(ctx context.Context, progress func(string)) (string, error)
}

// StepCheckDatabase is the name of the install step that checks the
// connection Secrets for an external database. A caller that sets up the
// database itself inserts its steps before this one.
const StepCheckDatabase = "Check database secrets"

// Run runs steps in order and reports each one. It stops at the first error.
func (in *Installer) Run(ctx context.Context, steps []Step) error {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	in.report.Report(Event{Kind: Plan, Steps: names})
	for _, s := range steps {
		in.report.Report(Event{Kind: StepStarted, Step: s.Name})
		start := time.Now()
		progress := func(msg string) {
			in.report.Report(Event{Kind: StepProgress, Step: s.Name, Msg: msg})
		}
		msg, err := s.Run(ctx, progress)
		elapsed := time.Since(start)
		if err != nil {
			in.report.Report(Event{Kind: StepFailed, Step: s.Name, Err: err, Elapsed: elapsed})
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		in.report.Report(Event{Kind: StepDone, Step: s.Name, Msg: msg, Elapsed: elapsed})
	}
	return nil
}

// Install runs InstallSteps. Rerunning it is safe and resumes a failed
// install: existing secrets are kept and existing releases are upgraded in
// place when their chart version matches.
func (in *Installer) Install(ctx context.Context) error {
	steps, err := in.InstallSteps()
	if err != nil {
		return err
	}
	return in.Run(ctx, steps)
}

// InstallSteps returns the steps that install every release in order.
func (in *Installer) InstallSteps() ([]Step, error) {
	if err := in.opts.validate(); err != nil {
		return nil, err
	}
	if in.opts.Postgres.Type == PostgresBundled {
		in.report.Report(Event{Kind: Note, Msg: "Installing a single-replica PostgreSQL for evaluation only. It has no backups or high availability. Set postgres.type to external with a managed database for anything else."})
	}
	steps := []Step{
		{"Check cluster", in.preflight},
		{"Install CRDs", in.helmStep(crdsRelease, noValues)},
		{"Create pod certificate CAs", in.createPodCertCAs},
		{"Install certificate controller", in.helmStep(podCertRelease, noValues)},
		{"Wait for trust bundles", in.waitTrustBundles},
		{"Create identity secrets", in.createIdentitySecrets},
		{"Configure object storage", in.configureObjectStore},
		{"Install atelet", in.helmStep(ateletRelease, in.ateletValues)},
	}
	if in.opts.Postgres.Type == PostgresBundled {
		steps = append(steps,
			Step{"Install PostgreSQL", in.applyPostgres},
			Step{"Set up database", in.setupDatabase},
		)
	} else {
		steps = append(steps, Step{StepCheckDatabase, in.checkDatabaseSecrets})
	}
	steps = append(steps, Step{"Install Substrate", in.helmStep(substrateRelease, in.substrateValues)})
	if in.opts.MicroVM.Enabled {
		steps = append(steps,
			Step{"Stage microVM assets", in.stageMicroVMAssets},
			Step{"Apply microVM SandboxConfig", in.applyMicroVMSandboxConfig},
		)
	}
	steps = append(steps, Step{"Check API", in.checkAPI})
	return steps, nil
}
