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
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"helm.sh/helm/v4/pkg/cli/values"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Config describes an installation. Install, upgrade and uninstall read it
// from the file passed to --config.
type Config struct {
	Postgres    PostgresMode `json:"postgres,omitempty"`
	ObjectStore ObjectStore  `json:"objectStore,omitempty"`
	MicroVM     MicroVM      `json:"microvm,omitzero"`
	// Timeout bounds each wait for workloads to become ready.
	Timeout metav1.Duration `json:"timeout,omitzero"`
	// Releases holds Helm values for the releases.
	Releases Releases `json:"releases,omitzero"`
}

// Releases holds Helm values. All applies to every release except
// substrate-crds, and each release's own values win over All.
type Releases struct {
	All       ReleaseValues `json:"all,omitzero"`
	PodCert   ReleaseValues `json:"podcert,omitzero"`
	Atelet    ReleaseValues `json:"atelet,omitzero"`
	Substrate ReleaseValues `json:"substrate,omitzero"`
}

// ReleaseValues follow helm's -f and --set: Values wins over ValuesFiles.
// Relative paths are relative to the config file.
type ReleaseValues struct {
	ValuesFiles []string       `json:"valuesFiles,omitempty"`
	Values      map[string]any `json:"values,omitempty"`
}

// MicroVM configures the microvm sandbox class.
type MicroVM struct {
	// Enabled stages the microVM guest assets and applies the microvm
	// SandboxConfig.
	Enabled bool `json:"enabled,omitempty"`
	// AssetBucket is the bucket for the assets with an s3 or gcs object
	// store. RustFS uses its snapshot bucket.
	AssetBucket string `json:"assetBucket,omitempty"`
}

// DefaultConfig is an evaluation install: bundled PostgreSQL and RustFS.
func DefaultConfig() Config {
	return Config{
		Postgres:    PostgresBundled,
		ObjectStore: ObjectStoreRustFS,
		Timeout:     metav1.Duration{Duration: 10 * time.Minute},
	}
}

// ReadConfig decodes a config from r and fills unset fields from
// DefaultConfig. dir resolves relative ValuesFiles.
func ReadConfig(r io.Reader, dir string) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, err
	}
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, errors.New("config is empty")
	}
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("while decoding config: %w", err)
	}
	for _, rv := range cfg.Releases.all() {
		for i, f := range rv.ValuesFiles {
			if !filepath.IsAbs(f) {
				rv.ValuesFiles[i] = filepath.Join(dir, f)
			}
		}
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	switch c.Postgres {
	case PostgresBundled, PostgresExternal:
	default:
		return fmt.Errorf("invalid postgres %q: must be %s or %s", c.Postgres, PostgresBundled, PostgresExternal)
	}
	switch c.ObjectStore {
	case ObjectStoreRustFS, ObjectStoreS3, ObjectStoreGCS:
	default:
		return fmt.Errorf("invalid objectStore %q: must be %s, %s or %s", c.ObjectStore, ObjectStoreRustFS, ObjectStoreS3, ObjectStoreGCS)
	}
	if c.MicroVM.Enabled && c.ObjectStore != ObjectStoreRustFS && c.MicroVM.AssetBucket == "" {
		return errors.New("microvm with an s3 or gcs objectStore needs microvm.assetBucket")
	}
	if c.Timeout.Duration <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}

func (r *Releases) all() []*ReleaseValues {
	return []*ReleaseValues{&r.All, &r.PodCert, &r.Atelet, &r.Substrate}
}

// userValues returns each release's values, keyed by release name: All's
// files and values, then the release's own.
func (c Config) userValues() (map[string]map[string]any, error) {
	all, err := c.Releases.All.read()
	if err != nil {
		return nil, err
	}
	user := map[string]map[string]any{}
	for r, rv := range map[release]ReleaseValues{
		podCertRelease:   c.Releases.PodCert,
		ateletRelease:    c.Releases.Atelet,
		substrateRelease: c.Releases.Substrate,
	} {
		own, err := rv.read()
		if err != nil {
			return nil, err
		}
		user[r.name] = mergeValues(all, own)
	}
	return user, nil
}

func (rv ReleaseValues) read() (map[string]any, error) {
	vo := values.Options{ValueFiles: rv.ValuesFiles}
	files, err := vo.MergeValues(nil)
	if err != nil {
		return nil, fmt.Errorf("while reading values: %w", err)
	}
	return mergeValues(files, rv.Values), nil
}
