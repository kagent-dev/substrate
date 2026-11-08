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
// matching config to w. askProfile asks for the profile, otherwise cfg's is
// kept. It returns install.ErrAborted if the user declines or quits.
func Wizard(w io.Writer, cfg *install.Config, askProfile bool) error {
	store := &cfg.ObjectStore
	external := func() bool { return store.Type != install.ObjectStoreBundled }
	aks := func() bool { return cfg.Profile == install.ProfileAKS }
	bundledVolumes := func() bool { return cfg.Postgres.Type == install.PostgresBundled || !external() }
	confirmed := true
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[install.Profile]().
				Title("Cluster").
				Options(
					huh.NewOption("Any Kubernetes cluster", install.ProfileGeneric),
					huh.NewOption("Azure Kubernetes Service (AKS)", install.ProfileAKS),
				).
				Value(&cfg.Profile),
		).WithHideFunc(func() bool { return !askProfile }),
		huh.NewGroup(
			huh.NewInput().
				Title("Azure Container Registry").
				Description("Login server to pull Agent Substrate's images from, like myregistry.azurecr.io. It must hold\nthe images under their usual paths, and the cluster must be attached to it. Leave empty for the public registries.").
				Value(&cfg.AKS.ACR).
				Validate(install.ValidateACR),
			huh.NewInput().
				Title("Node pool").
				Description("Runs atelet only in this pool. WorkerPools then need the nodeSelector\nkubernetes.azure.com/agentpool set to it. Leave empty for every node.").
				Value(&cfg.AKS.NodePool).
				Validate(install.ValidateNodePool),
			huh.NewConfirm().
				Title("Pull actor images from ACR?").
				Description("Runs the node's acr-credential-provider in atelet. atelet reads the node's\n/etc/kubernetes/azure.json and gets every registry the kubelet identity can pull from.\nOn a service principal cluster, that file holds the service principal's secret.").
				Value(&cfg.AKS.ACRCredentialProvider),
		).WithHideFunc(func() bool { return !aks() }),
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
				Title("StorageClass").
				Description("For the bundled volumes. Leave empty for the cluster's default.").
				PlaceholderFunc(func() string { return strings.Join(storageClassSuggestions(cfg.Profile), ", ") }, &cfg.Profile).
				SuggestionsFunc(func() []string { return storageClassSuggestions(cfg.Profile) }, &cfg.Profile).
				Value(&cfg.StorageClass).
				Validate(install.ValidateStorageClass),
		).WithHideFunc(func() bool { return !bundledVolumes() }),
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
	if !bundledVolumes() {
		cfg.StorageClass = ""
	}
	if !aks() {
		cfg.AKS = install.AKS{}
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

// storageClassSuggestions lists the StorageClasses to suggest on profile p.
func storageClassSuggestions(p install.Profile) []string {
	if p == install.ProfileAKS {
		return []string{"managed-csi-premium"}
	}
	return nil
}

func notEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	return nil
}
