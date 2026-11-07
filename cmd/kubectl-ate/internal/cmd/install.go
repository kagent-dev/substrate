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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"go.ate.dev/substrate/cmd/kubectl-ate/internal/install"
)

var (
	installOpts   = install.Options{Postgres: install.PostgresBundled, ObjectStore: install.ObjectStoreRustFS}
	upgradeOpts   install.Options
	uninstallOpts install.Options
	uninstallYes  bool
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
object store, and applies the microvm SandboxConfig. Upgrade restages them.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runInstaller(cmd, installOpts, (*install.Installer).Install)
	},
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade Agent Substrate to the charts in this kubectl ate",
	Long: `Upgrade every Agent Substrate release to the chart version built into this
kubectl ate: the certificate controller, atelet, the CRDs, then the substrate
release. It doesn't roll WorkerPool worker images, which
docs/reference/version-skew-policy.md upgrades after atelet.

Each release keeps the values it was installed with. Values passed with -f and
--set are layered on top. Upgrade refuses downgrades, skipped minor releases,
and CRD changes that would strand stored objects.

--dry-run prints each release's manifest diff and changes nothing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts := upgradeOpts
		opts.Out = cmd.OutOrStdout()
		return runInstaller(cmd, opts, (*install.Installer).Upgrade)
	},
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall Agent Substrate",
	Long: `Uninstall Agent Substrate from the current cluster.

Deletes every WorkerPool first, so ate-controller can finalize them, then
uninstalls the releases in the reverse of install order and removes the
bundled PostgreSQL.

CRDs, CA keys, secrets, the database volume and the RustFS volume stay unless
--include-crds or --purge are set, so a later install keeps its identities and
data.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts := uninstallOpts
		if !uninstallYes {
			opts.Confirm = confirmFunc(cmd)
		}
		return runInstaller(cmd, opts, (*install.Installer).Uninstall)
	},
}

// confirmFunc asks on the terminal. Without one it refuses, so scripts must
// pass --yes. Ctrl+C cancels ctx, which ends the wait for an answer.
func confirmFunc(cmd *cobra.Command) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, summary string) (bool, error) {
		w := cmd.ErrOrStderr()
		fmt.Fprint(w, summary)
		f, ok := cmd.InOrStdin().(*os.File)
		if !ok || !isTerminal(f) {
			return false, errors.New("stdin isn't a terminal; pass --yes to uninstall")
		}
		fmt.Fprint(w, "Uninstall? [y/N] ")
		type line struct {
			s   string
			err error
		}
		read := make(chan line, 1)
		go func() {
			s, err := bufio.NewReader(f).ReadString('\n')
			read <- line{s, err}
		}()
		var l line
		select {
		case <-ctx.Done():
			fmt.Fprintln(w)
			return false, ctx.Err()
		case l = <-read:
		}
		if l.err != nil && !errors.Is(l.err, io.EOF) {
			return false, l.err
		}
		answer := strings.ToLower(strings.TrimSpace(l.s))
		return answer == "y" || answer == "yes", nil
	}
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

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// addChartFlags adds the flags install and upgrade share.
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
	addChartFlags(upgradeCmd.Flags(), &upgradeOpts)
	upgradeCmd.Flags().BoolVar(&upgradeOpts.DryRun, "dry-run", false, "Print each release's manifest diff without upgrading")
	f := uninstallCmd.Flags()
	f.BoolVar(&uninstallOpts.IncludeCRDs, "include-crds", false, "Also delete the CRDs, with every WorkerPool, SandboxConfig and CSIDriverConfig")
	f.BoolVar(&uninstallOpts.Purge, "purge", false, "Also delete the namespaces, CA keys, secrets, database volume and trust bundles")
	f.BoolVarP(&uninstallYes, "yes", "y", false, "Don't ask for confirmation")
	f.DurationVar(&uninstallOpts.Timeout, "timeout", 10*time.Minute, "How long to wait for each step")
	rootCmd.AddCommand(installCmd, upgradeCmd, uninstallCmd, statusCmd)
}
