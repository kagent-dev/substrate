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

package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"sigs.k8s.io/yaml"

	"go.ate.dev/substrate/cmd/kubectl-ate/internal/install"
)

// Wizard asks for the install choices, stores them in cfg, and prints the
// matching config to w. It returns install.ErrAborted if the user declines or
// quits.
func Wizard(w io.Writer, cfg *install.Config) error {
	var valuesFile string
	if len(cfg.Releases.All.ValuesFiles) > 0 {
		valuesFile = cfg.Releases.All.ValuesFiles[0]
	}
	external := func() bool { return cfg.ObjectStore != install.ObjectStoreRustFS }
	confirmed := true
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[install.PostgresMode]().
				Title("PostgreSQL").
				Options(
					huh.NewOption("Bundled: single replica, for evaluation only", install.PostgresBundled),
					huh.NewOption("External: I created the connection Secrets", install.PostgresExternal),
				).
				Value(&cfg.Postgres),
			huh.NewSelect[install.ObjectStore]().
				Title("Snapshot storage").
				Options(
					huh.NewOption("RustFS in the cluster", install.ObjectStoreRustFS),
					huh.NewOption("Amazon S3 or compatible", install.ObjectStoreS3),
					huh.NewOption("Google Cloud Storage", install.ObjectStoreGCS),
				).
				Value(&cfg.ObjectStore),
			huh.NewConfirm().
				Title("Install the microVM sandbox class?").
				Description("Downloads the Kata and Cloud Hypervisor guest assets and stages them in the object store.").
				Value(&cfg.MicroVM.Enabled),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Values file").
				Description("Sets objectStore.*, ateApi.snapshotLocation and, for GCS, objectStorePlugin.transferServiceAccount.").
				Value(&valuesFile).
				Validate(fileExists),
		).WithHideFunc(func() bool { return !external() }),
		huh.NewGroup(
			huh.NewInput().
				Title("MicroVM asset bucket").
				Description("atelet must be able to read it.").
				Value(&cfg.MicroVM.AssetBucket).
				Validate(notEmpty),
		).WithHideFunc(func() bool { return !external() || !cfg.MicroVM.Enabled }),
		huh.NewGroup(
			huh.NewConfirm().
				Title("Install Agent Substrate?").
				Affirmative("Install").
				Negative("Cancel").
				Value(&confirmed),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return install.ErrAborted
		}
		return err
	}
	if !confirmed {
		return install.ErrAborted
	}
	if external() {
		cfg.Releases.All.ValuesFiles = []string{valuesFile}
	} else {
		cfg.Releases.All.ValuesFiles = nil
	}
	if !external() || !cfg.MicroVM.Enabled {
		cfg.MicroVM.AssetBucket = ""
	}
	hint, err := Hint(cfg)
	if err != nil {
		return err
	}
	fmt.Fprint(w, dimSt.Render(hint))
	return nil
}

// Hint tells the user how to rerun install with cfg and no questions.
func Hint(cfg *install.Config) (string, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return "To skip these questions next time, save this as install.yaml and run kubectl ate install -c install.yaml:\n\n" + string(data) + "\n", nil
}

func fileExists(p string) error {
	if p == "" {
		return errors.New("required")
	}
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("can't read %s", p)
	}
	return nil
}

func notEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	return nil
}
