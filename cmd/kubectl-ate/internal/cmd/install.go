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
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"go.ate.dev/substrate/pkg/kagent/install"
	"go.ate.dev/substrate/pkg/kagent/install/tui"
)

var (
	configPath    string
	chartDir      string
	noTUI         bool
	upgradeDryRun bool
	uninstallOpts install.UninstallOptions
	uninstallYes  bool
)

const configHelp = `--config takes a YAML file, or - for stdin:

  postgres:
    type: bundled          # or external
  objectStore:
    type: bundled          # or s3, gcs
    bucket: my-snapshots   # for s3 or gcs
    s3:                    # all optional
      region: us-east-1
      endpoint: https://s3.example.com
      pathStyle: true
      credentialsSecret: s3-creds # in the system namespace
    gcs:
      transferServiceAccount: transfer@my-project.iam.gserviceaccount.com
  microvm:
    enabled: true
    assetBucket: my-assets # for s3 or gcs
  timeout: 10m             # each wait for workloads
  releases:                # Helm values
    all:                   # every release except substrate-crds
      valuesFiles:         # relative to the config file
      - gke.yaml
      values:              # wins over valuesFiles
        image:
          tag: v1.0.0
    podcert: {}            # substrate-podcert
    atelet: {}             # substrate-atelet
    substrate:             # substrate
      values:
        otel:
          endpoint: http://otel-collector:4317

A release's own valuesFiles and values win over the ones in all.`

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Agent Substrate from its Helm charts",
	Long: `Install Agent Substrate into the current cluster.

Installs the substrate-crds, substrate-podcert, substrate-atelet and substrate
Helm releases in order, and creates the CA pools and secrets they need.
Existing secrets are kept. Rerunning install resumes a failed install.

Without --config, install uses a single-replica PostgreSQL for evaluation and
a RustFS for snapshots. On a terminal it asks for these choices first and
prints the matching config.

For anything but evaluation, create the database connection Secrets
described in charts/substrate/README.md and set postgres.type to external.
To store snapshots in a bucket, set objectStore.type to s3 or gcs and set
objectStore.bucket. GCS also needs objectStore.gcs.transferServiceAccount.

microvm.enabled downloads the Kata and Cloud Hypervisor guest assets for each
node architecture, caches them under the user cache directory, uploads them to
the object store, and applies the microvm SandboxConfig. Upgrade restages them.

` + configHelp,
	Example: `  kubectl ate install -c install.yaml
  kubectl ate install -c - <<EOF
  microvm:
    enabled: true
  EOF`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts, tty, err := options(cmd)
		if err != nil {
			return err
		}
		defer closeTerminal(tty)
		if useTUI(cmd, tty) && configPath == "" {
			if err := tui.Wizard(cmd.ErrOrStderr(), &opts.Config); err != nil {
				return err
			}
		}
		return runInstaller(cmd, opts, tty, "install", (*install.Installer).Install,
			"Agent Substrate is installed.\nCheck it with: kubectl ate status")
	},
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade Agent Substrate to the charts in this kubectl ate",
	Long: `Upgrade every Agent Substrate release to the chart version built into this
kubectl ate: the certificate controller, atelet, the CRDs, then the substrate
release. It doesn't roll WorkerPool worker images, which
docs/reference/version-skew-policy.md upgrades after atelet.

Each release keeps the values it was installed with. The config's
releases values are layered on top. Upgrade also reads timeout and
microvm.assetBucket from the config. It refuses downgrades, skipped minor
releases, and CRD changes that would strand stored objects.

--dry-run prints each release's manifest diff and changes nothing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts, tty, err := options(cmd)
		if err != nil {
			return err
		}
		defer closeTerminal(tty)
		up := install.UpgradeOptions{DryRun: upgradeDryRun, Out: cmd.OutOrStdout()}
		// The diff goes to stdout, so it can't share the terminal with the TUI.
		if up.DryRun {
			tty = nil
		}
		return runInstaller(cmd, opts, tty, "upgrade", func(in *install.Installer, ctx context.Context) error {
			return in.Upgrade(ctx, up)
		}, "Agent Substrate is upgraded.")
	},
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall Agent Substrate",
	Long: `Uninstall Agent Substrate from the current cluster.

Deletes every WorkerPool first, so ate-controller can finalize them, then
uninstalls the releases in the reverse of install order and removes the
bundled PostgreSQL. It reads only timeout from the config.

CRDs, CA keys, secrets, the database volume and the RustFS volume stay unless
--include-crds or --purge are set, so a later install keeps its identities and
data.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts, tty, err := options(cmd)
		if err != nil {
			return err
		}
		defer closeTerminal(tty)
		un := uninstallOpts
		if !uninstallYes {
			un.Confirm = confirmFunc(cmd, tty)
		}
		return runInstaller(cmd, opts, tty, "uninstall", func(in *install.Installer, ctx context.Context) error {
			return in.Uninstall(ctx, un)
		}, "Agent Substrate is uninstalled.")
	},
}

// options reads the config and finds the terminal to read keys and answers
// from. Without --config the config is install.DefaultConfig. The terminal is
// nil when there is none. Close it with closeTerminal.
func options(cmd *cobra.Command) (install.Options, *os.File, error) {
	var opts install.Options
	if chartDir != "" {
		opts.Charts = os.DirFS(chartDir)
	}
	var err error
	switch configPath {
	case "":
		opts.Config = install.DefaultConfig()
	case "-":
		opts.Config, err = install.ReadConfig(cmd.InOrStdin(), ".")
	default:
		var f *os.File
		if f, err = os.Open(configPath); err == nil {
			opts.Config, err = install.ReadConfig(f, filepath.Dir(configPath))
			f.Close()
		}
	}
	if err != nil {
		return opts, nil, fmt.Errorf("while reading %s: %w", configName(), err)
	}
	return opts, terminal(cmd), nil
}

func configName() string {
	if configPath == "-" {
		return "config from stdin"
	}
	return configPath
}

// terminal returns stdin if it's a terminal. When the config came from stdin
// it opens the controlling terminal instead. It returns nil if neither works.
func terminal(cmd *cobra.Command) *os.File {
	if configPath == "-" {
		f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return nil
		}
		return f
	}
	if f, ok := cmd.InOrStdin().(*os.File); ok && isTerminal(f) {
		return f
	}
	return nil
}

func closeTerminal(tty *os.File) {
	if tty != nil && configPath == "-" {
		tty.Close()
	}
}

// confirmFunc asks on tty. Without one it refuses, so scripts must pass
// --yes. Ctrl+C cancels ctx, which ends the wait for an answer.
func confirmFunc(cmd *cobra.Command, tty *os.File) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, summary string) (bool, error) {
		w := cmd.ErrOrStderr()
		fmt.Fprint(w, summary)
		if tty == nil {
			return false, errors.New("no terminal to ask on; pass --yes to uninstall")
		}
		fmt.Fprint(w, "Uninstall? [y/N] ")
		type line struct {
			s   string
			err error
		}
		read := make(chan line, 1)
		go func() {
			s, err := bufio.NewReader(tty).ReadString('\n')
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

// runInstaller runs fn with the TUI when useTUI allows it, and with plain
// lines otherwise. The TUI shows the error itself, so the returned error is
// marked as reported.
func runInstaller(cmd *cobra.Command, opts install.Options, tty *os.File, title string, fn func(*install.Installer, context.Context) error, summary string) error {
	if !useTUI(cmd, tty) {
		in, err := newInstaller(cmd, opts)
		if err != nil {
			return err
		}
		return fn(in, cmd.Context())
	}
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer cancel()
	r := tui.New(tty, cmd.ErrOrStderr(), title, cancel)
	in, err := newInstallerWith(cmd, opts, r)
	if err != nil {
		return err
	}
	err = fn(in, ctx)
	if r.Close(err, summary) && err != nil {
		return reportedError{err}
	}
	return err
}

// useTUI reports whether there is a terminal for keys, stderr is a terminal,
// and nothing asked for plain output. Helm logs at -v would interleave with
// the TUI.
func useTUI(cmd *cobra.Command, tty *os.File) bool {
	return !noTUI && verbosity == 0 && tty != nil && isTerminal(cmd.ErrOrStderr())
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func init() {
	for _, c := range []*cobra.Command{installCmd, upgradeCmd, uninstallCmd} {
		c.Flags().StringVarP(&configPath, "config", "c", "", "Install config file, or - for stdin")
		c.Flags().BoolVar(&noTUI, "no-tui", false, "Print plain progress lines instead of the interactive view")
	}
	for _, c := range []*cobra.Command{installCmd, upgradeCmd} {
		c.Flags().StringVar(&chartDir, "chart-dir", "", "Load charts from this directory instead of the ones built into kubectl ate")
	}
	upgradeCmd.Flags().BoolVar(&upgradeDryRun, "dry-run", false, "Print each release's manifest diff without upgrading")
	f := uninstallCmd.Flags()
	f.BoolVar(&uninstallOpts.IncludeCRDs, "include-crds", false, "Also delete the CRDs, with every WorkerPool, SandboxConfig and CSIDriverConfig")
	f.BoolVar(&uninstallOpts.Purge, "purge", false, "Also delete the namespaces, CA keys, secrets, database volume and trust bundles")
	f.BoolVarP(&uninstallYes, "yes", "y", false, "Don't ask for confirmation")
	rootCmd.AddCommand(installCmd, upgradeCmd, uninstallCmd, statusCmd)
}
