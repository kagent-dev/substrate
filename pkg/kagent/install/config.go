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
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"helm.sh/helm/v4/pkg/cli/values"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// profileFS holds the built-in profiles, one config file each.
//
//go:embed profiles/*.yaml
var profileFS embed.FS

// Config describes an installation. Install, upgrade and uninstall read it
// from the file passed to --config.
type Config struct {
	Postgres    Postgres    `json:"postgres,omitzero"`
	ObjectStore ObjectStore `json:"objectStore,omitzero"`
	MicroVM     MicroVM     `json:"microvm,omitzero"`
	// ImageRegistry is a registry to pull every image kubectl ate installs
	// from, like myregistry.azurecr.io. It must hold the images under their
	// usual paths. Empty keeps each image's own registry.
	ImageRegistry string `json:"imageRegistry,omitempty"`
	// StorageClass is the StorageClass of the bundled PostgreSQL and RustFS
	// volumes. Empty uses the cluster's default.
	StorageClass string `json:"storageClass,omitempty"`
	// OTelEndpoint is an existing OTLP collector that atelet and the control
	// plane export traces, metrics and logs to. Empty leaves the charts' otel
	// values as they are.
	OTelEndpoint string `json:"otelEndpoint,omitempty"`
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

// Postgres configures the control plane's database. External uses the
// connection Secrets described in charts/substrate/README.md.
type Postgres struct {
	Type PostgresType `json:"type,omitempty"`
}

// UnmarshalJSON also accepts the type alone, as in postgres: external.
func (p *Postgres) UnmarshalJSON(b []byte) error {
	var t PostgresType
	if json.Unmarshal(b, &t) == nil {
		*p = Postgres{Type: t}
		return nil
	}
	type plain Postgres
	return decodeStrict(b, (*plain)(p))
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
	if json.Unmarshal(b, &t) == nil {
		*o = ObjectStore{Type: t}
		return nil
	}
	type plain ObjectStore
	return decodeStrict(b, (*plain)(o))
}

// UnmarshalJSON reads rustfs, the v1.0.0 name, as bundled.
func (t *ObjectStoreType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "rustfs" {
		s = string(ObjectStoreBundled)
	}
	*t = ObjectStoreType(s)
	return nil
}

// decodeStrict decodes b into v, keeping the fields b doesn't set.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (o ObjectStore) validate() error {
	switch o.Type {
	case ObjectStoreBundled:
		if o != (ObjectStore{Type: ObjectStoreBundled}) {
			return errors.New("objectStore bundled takes no other settings")
		}
	case ObjectStoreS3, ObjectStoreGCS:
	default:
		return fmt.Errorf("invalid objectStore.type %q: must be %s, %s or %s", o.Type, ObjectStoreBundled, ObjectStoreS3, ObjectStoreGCS)
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
		Postgres:    Postgres{Type: PostgresBundled},
		ObjectStore: ObjectStore{Type: ObjectStoreBundled},
		Timeout:     metav1.Duration{Duration: 10 * time.Minute},
	}
}

// Profiles lists the built-in profiles. Each is a config file that ReadConfig
// and ProfileConfig layer a config over.
func Profiles() []string {
	entries, err := profileFS.ReadDir("profiles")
	if err != nil {
		panic(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	return names
}

// ReadConfig decodes a config from r and fills unset fields from the profile,
// then from DefaultConfig. An empty profile is none. dir resolves relative
// ValuesFiles.
func ReadConfig(r io.Reader, dir string, profile string) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, errors.New("config is empty")
	}
	var user map[string]any
	if err := yaml.UnmarshalStrict(data, &user); err != nil {
		return Config{}, fmt.Errorf("while decoding config: %w", err)
	}
	cfg, err := layerConfig(profile, user)
	if err != nil {
		return Config{}, err
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

// ProfileConfig returns DefaultConfig with the profile's settings. An empty
// profile is DefaultConfig.
func ProfileConfig(profile string) (Config, error) {
	cfg, err := layerConfig(profile, nil)
	if err != nil {
		return Config{}, err
	}
	return cfg, cfg.validate()
}

// layerConfig decodes user over the profile over DefaultConfig. Maps merge
// and lists replace, as in Helm values.
func layerConfig(profile string, user map[string]any) (Config, error) {
	var base map[string]any
	if profile != "" {
		data, err := profileFS.ReadFile("profiles/" + profile + ".yaml")
		if err != nil {
			return Config{}, fmt.Errorf("invalid profile %q: must be one of %v", profile, Profiles())
		}
		if err := yaml.UnmarshalStrict(data, &base); err != nil {
			return Config{}, fmt.Errorf("while decoding profile %s: %w", profile, err)
		}
	}
	data, err := json.Marshal(mergeValues(base, user))
	if err != nil {
		return Config{}, err
	}
	cfg := DefaultConfig()
	if err := decodeStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("while decoding config: %w", err)
	}
	return cfg, nil
}

// validateImageRegistry checks that s is empty or a registry host.
func validateImageRegistry(s string) error {
	if s == "" {
		return nil
	}
	// Image references read a first component without a dot or a port as a
	// Docker Hub namespace.
	if !strings.ContainsAny(s, ".:") || strings.ContainsFunc(s, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) {
		return fmt.Errorf("invalid imageRegistry %q: must be a registry host like myregistry.azurecr.io", s)
	}
	return nil
}

// ValidateStorageClass checks that s is empty or a valid StorageClass name.
func ValidateStorageClass(s string) error {
	if s == "" {
		return nil
	}
	if errs := validation.IsDNS1123Subdomain(s); len(errs) > 0 {
		return fmt.Errorf("invalid StorageClass name %q: %s", s, strings.Join(errs, "; "))
	}
	return nil
}

// ValidateOTelEndpoint checks that s is empty or an http or https URL.
func ValidateOTelEndpoint(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid OTLP endpoint %q: must be a URL like http://otel-collector:4317", s)
	}
	return nil
}

func (c Config) validate() error {
	if err := validateImageRegistry(c.ImageRegistry); err != nil {
		return err
	}
	switch c.Postgres.Type {
	case PostgresBundled, PostgresExternal:
	default:
		return fmt.Errorf("invalid postgres.type %q: must be %s or %s", c.Postgres.Type, PostgresBundled, PostgresExternal)
	}
	if err := c.ObjectStore.validate(); err != nil {
		return err
	}
	if err := ValidateStorageClass(c.StorageClass); err != nil {
		return fmt.Errorf("storageClass: %w", err)
	}
	if err := ValidateOTelEndpoint(c.OTelEndpoint); err != nil {
		return fmt.Errorf("otelEndpoint: %w", err)
	}
	if c.MicroVM.Enabled && c.ObjectStore.Type != ObjectStoreBundled && c.MicroVM.AssetBucket == "" {
		return errors.New("microvm with an s3 or gcs objectStore needs microvm.assetBucket")
	}
	if c.Timeout.Duration <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}

// registryValues returns the Helm values that pull a release's images from
// ImageRegistry.
func (c Config) registryValues() map[string]any {
	if c.ImageRegistry == "" {
		return nil
	}
	return map[string]any{"global": map[string]any{"imageRegistry": c.ImageRegistry}}
}

func (r *Releases) all() []*ReleaseValues {
	return []*ReleaseValues{&r.All, &r.PodCert, &r.Atelet, &r.Substrate}
}

// otelValues returns the Helm values that point r at OTelEndpoint.
func (c Config) otelValues(r release) map[string]any {
	if c.OTelEndpoint == "" || (r != ateletRelease && r != substrateRelease) {
		return nil
	}
	return map[string]any{"otel": map[string]any{"endpoint": c.OTelEndpoint}}
}

// userValues returns each release's values, keyed by release name:
// ImageRegistry's and OTelEndpoint's, All's files and values, then the
// release's own.
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
		user[r.name] = mergeValues(c.registryValues(), c.otelValues(r), all, own)
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
