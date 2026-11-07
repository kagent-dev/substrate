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

package cmd

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.ate.dev/substrate/cmd/kubectl-ate/internal/install"
)

var (
	installOpts = install.Options{Postgres: install.PostgresBundled, ObjectStore: install.ObjectStoreRustFS}
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Agent Substrate from its Helm charts",
	Long: `Install Agent Substrate into the current cluster.

Installs the substrate-crds, substrate-podcert, substrate-atelet and substrate
Helm releases in order, and creates the CA pools and secrets they need.
Existing secrets are kept. Rerunning install resumes a failed install.

By default it also installs a single-replica PostgreSQL for evaluation. For
anything else, create the database connection Secrets described in
charts/substrate/README.md and pass --postgres external.

Snapshots go to a RustFS the substrate release runs. To use a bucket instead,
pass --object-store s3 or gcs with a values file that sets objectStore.*,
ateApi.snapshotLocation and, for GCS, objectStorePlugin.transferServiceAccount.

--microvm downloads the Kata and Cloud Hypervisor guest assets for each node
architecture, caches them under the user cache directory, uploads them to the
object store, and applies the microvm SandboxConfig.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runInstaller(cmd, installOpts, (*install.Installer).Install)
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the installed Agent Substrate releases and workloads",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		in, err := newInstaller(cmd, install.Options{})
		if err != nil {
			return err
		}
		return in.Status(cmd.Context(), cmd.OutOrStdout())
	},
}

func newInstaller(cmd *cobra.Command, opts install.Options) (*install.Installer, error) {
	return newInstallerWith(cmd, opts, install.PlainReporter{W: cmd.ErrOrStderr()})
}

func newInstallerWith(cmd *cobra.Command, opts install.Options, r install.Reporter) (*install.Installer, error) {
	opts.Kubeconfig, opts.Context = kubeconfig, k8sContext
	if verbosity > 0 {
		opts.HelmLog = cmd.ErrOrStderr()
	}
	return install.New(opts, r)
}

func runInstaller(cmd *cobra.Command, opts install.Options, fn func(*install.Installer, context.Context) error) error {
	in, err := newInstaller(cmd, opts)
	if err != nil {
		return err
	}
	return fn(in, cmd.Context())
}

// addChartFlags adds the flags that select charts and values.
func addChartFlags(f *pflag.FlagSet, opts *install.Options) {
	f.StringVar(&opts.ChartDir, "chart-dir", "", "Load charts from this directory instead of the ones built into kubectl ate")
	f.StringArrayVarP(&opts.ValuesFiles, "values", "f", nil, "Values file for every release except substrate-crds (repeatable)")
	f.StringArrayVar(&opts.Values, "set", nil, "Set a value for every release except substrate-crds, as key=value (repeatable)")
	f.DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "How long to wait for each release's workloads")
}

func init() {
	addChartFlags(installCmd.Flags(), &installOpts)
	installCmd.Flags().StringVar((*string)(&installOpts.Postgres), "postgres", string(install.PostgresBundled), "PostgreSQL to use: bundled (evaluation only) or external")
	installCmd.Flags().StringVar((*string)(&installOpts.ObjectStore), "object-store", string(install.ObjectStoreRustFS), "Snapshot storage: rustfs (in cluster), s3 or gcs")
	installCmd.Flags().BoolVar(&installOpts.MicroVM, "microvm", false, "Also install the microvm sandbox class: download its guest assets, stage them in the object store and apply the microvm SandboxConfig")
	installCmd.Flags().StringVar(&installOpts.MicroVMAssetBucket, "microvm-asset-bucket", "", "Bucket for microVM assets with --object-store s3 or gcs; atelet must be able to read it")
	rootCmd.AddCommand(installCmd, statusCmd)
}
