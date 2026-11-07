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
	// ValuesFiles and Values apply to every release except substrate-crds.
	// Values wins over ValuesFiles. Relative paths are relative to the
	// config file.
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
	for i, f := range cfg.ValuesFiles {
		if !filepath.IsAbs(f) {
			cfg.ValuesFiles[i] = filepath.Join(dir, f)
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

// userValues reads ValuesFiles and merges Values on top.
func (c Config) userValues() (map[string]any, error) {
	vo := values.Options{ValueFiles: c.ValuesFiles}
	files, err := vo.MergeValues(nil)
	if err != nil {
		return nil, fmt.Errorf("while reading values: %w", err)
	}
	return mergeValues(files, c.Values), nil
}
