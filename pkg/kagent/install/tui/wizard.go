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
	"strings"

	"github.com/charmbracelet/huh"
	"sigs.k8s.io/yaml"

	"go.ate.dev/substrate/pkg/kagent/install"
)

// Wizard asks for the install choices, stores them in cfg, and prints the
// matching config to w. It returns install.ErrAborted if the user declines or
// quits.
func Wizard(w io.Writer, cfg *install.Config) error {
	store := &cfg.ObjectStore
	external := func() bool { return store.Type != install.ObjectStoreBundled }
	confirmed := true
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[install.PostgresType]().
				Title("PostgreSQL").
				Options(
					huh.NewOption("Bundled: single replica, for evaluation only", install.PostgresBundled),
					huh.NewOption("External: I created the connection Secrets", install.PostgresExternal),
				).
				Value(&cfg.Postgres.Type),
			huh.NewSelect[install.ObjectStoreType]().
				Title("Snapshot storage").
				Options(
					huh.NewOption("RustFS in the cluster", install.ObjectStoreBundled),
					huh.NewOption("Amazon S3 or compatible", install.ObjectStoreS3),
					huh.NewOption("Google Cloud Storage", install.ObjectStoreGCS),
				).
				Value(&store.Type),
			huh.NewConfirm().
				Title("Install the microVM sandbox class?").
				Description("Downloads the Kata and Cloud Hypervisor guest assets and stages them in the object store.").
				Value(&cfg.MicroVM.Enabled),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Snapshot bucket").
				Value(&store.Bucket).
				Validate(notEmpty),
		).WithHideFunc(func() bool { return !external() }),
		huh.NewGroup(
			huh.NewInput().
				Title("S3 region").
				Description("Empty uses the AWS SDK default.").
				Value(&store.S3.Region),
			huh.NewInput().
				Title("S3 endpoint").
				Description("URL of an S3-compatible service. Empty uses AWS.").
				Value(&store.S3.Endpoint),
			huh.NewInput().
				Title("Credentials Secret").
				Description("Secret in "+install.SystemNamespace+" with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Empty uses ambient credentials.").
				Value(&store.S3.CredentialsSecret),
		).WithHideFunc(func() bool { return store.Type != install.ObjectStoreS3 }),
		huh.NewGroup(
			huh.NewInput().
				Title("Transfer service account").
				Description("Google service account the snapshot exchange impersonates.").
				Value(&store.GCS.TransferServiceAccount).
				Validate(notEmpty),
		).WithHideFunc(func() bool { return store.Type != install.ObjectStoreGCS }),
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
	// Drop answers to questions the final choices hid.
	if !external() {
		store.Bucket = ""
	}
	if store.Type != install.ObjectStoreS3 {
		store.S3 = install.S3ObjectStore{}
	}
	if store.Type != install.ObjectStoreGCS {
		store.GCS = install.GCSObjectStore{}
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

func notEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	return nil
}
