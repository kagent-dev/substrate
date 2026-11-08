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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/cli/values"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Config describes an installation. Install, upgrade and uninstall read it
// from the file passed to --config.
type Config struct {
	Postgres    PostgresMode `json:"postgres,omitempty"`
	ObjectStore ObjectStore  `json:"objectStore,omitzero"`
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

// ObjectStore configures snapshot storage. The s3 and gcs settings fill the
// charts' storage values on install, over the values in Releases.
type ObjectStore struct {
	Type ObjectStoreType `json:"type,omitempty"`
	// Bucket holds snapshots with an s3 or gcs store. It sets
	// ateApi.snapshotLocation to gs://<bucket>/, the URI both backends use.
	// Leave it empty to set snapshot locations per template with the
	// PerTemplateSnapshotLocation preview gate.
	Bucket string         `json:"bucket,omitempty"`
	S3     S3ObjectStore  `json:"s3,omitzero"`
	GCS    GCSObjectStore `json:"gcs,omitzero"`
}

// S3ObjectStore configures an s3 store. Empty fields use the AWS SDK defaults.
type S3ObjectStore struct {
	Region string `json:"region,omitempty"`
	// Endpoint is the URL of an S3-compatible service.
	Endpoint  string `json:"endpoint,omitempty"`
	PathStyle bool   `json:"pathStyle,omitempty"`
	// CredentialsSecret names a Secret in SystemNamespace with
	// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY keys. Empty uses the
	// pods' ambient credentials.
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// GCSObjectStore configures a gcs store.
type GCSObjectStore struct {
	// TransferServiceAccount is the Google service account the snapshot
	// exchange impersonates for actor-scoped transfers.
	TransferServiceAccount string `json:"transferServiceAccount,omitempty"`
}

// UnmarshalJSON also accepts the type alone, as in objectStore: s3.
func (o *ObjectStore) UnmarshalJSON(b []byte) error {
	var t ObjectStoreType
	if err := json.Unmarshal(b, &t); err == nil {
		*o = ObjectStore{Type: t}
		return nil
	}
	type plain ObjectStore
	p := plain(*o)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*o = ObjectStore(p)
	return nil
}

func (o ObjectStore) validate() error {
	switch o.Type {
	case ObjectStoreRustFS:
		if o != (ObjectStore{Type: ObjectStoreRustFS}) {
			return errors.New("objectStore rustfs takes no other settings")
		}
	case ObjectStoreS3, ObjectStoreGCS:
	default:
		return fmt.Errorf("invalid objectStore.type %q: must be %s, %s or %s", o.Type, ObjectStoreRustFS, ObjectStoreS3, ObjectStoreGCS)
	}
	if strings.ContainsAny(o.Bucket, ":/") {
		return fmt.Errorf("objectStore.bucket %q must be a bucket name, not a URI", o.Bucket)
	}
	if o.S3 != (S3ObjectStore{}) && o.Type != ObjectStoreS3 {
		return errors.New("objectStore.s3 needs type s3")
	}
	if o.GCS != (GCSObjectStore{}) && o.Type != ObjectStoreGCS {
		return errors.New("objectStore.gcs needs type gcs")
	}
	if e := o.S3.Endpoint; e != "" && !strings.HasPrefix(e, "http://") && !strings.HasPrefix(e, "https://") {
		return fmt.Errorf("objectStore.s3.endpoint %q must be an http or https URL", e)
	}
	return nil
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
		ObjectStore: ObjectStore{Type: ObjectStoreRustFS},
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
	if err := c.ObjectStore.validate(); err != nil {
		return err
	}
	if c.MicroVM.Enabled && c.ObjectStore.Type != ObjectStoreRustFS && c.MicroVM.AssetBucket == "" {
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
