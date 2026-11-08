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
	"slices"
	"strings"
	"time"
	"unicode"

	"helm.sh/helm/v4/pkg/cli/values"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// Config describes an installation. Install, upgrade and uninstall read it
// from the file passed to --config.
type Config struct {
	// Profile selects the kind of cluster. Each profile but generic has its
	// own section of options.
	Profile     Profile     `json:"profile,omitempty"`
	AKS         AKS         `json:"aks,omitzero"`
	Postgres    Postgres    `json:"postgres,omitzero"`
	ObjectStore ObjectStore `json:"objectStore,omitzero"`
	MicroVM     MicroVM     `json:"microvm,omitzero"`
	// StorageClass is the StorageClass of the bundled PostgreSQL and RustFS
	// volumes. Empty uses the cluster's default.
	StorageClass string `json:"storageClass,omitempty"`
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

// Profile selects the kind of cluster to install on.
type Profile string

const (
	// ProfileGeneric makes no assumptions about the cluster.
	ProfileGeneric Profile = "generic"
	// ProfileAKS installs on Azure Kubernetes Service.
	ProfileAKS Profile = "aks"
)

// Profiles lists the valid profiles.
var Profiles = []Profile{ProfileGeneric, ProfileAKS}

// AKS configures the aks profile.
type AKS struct {
	// ACR is the login server of an Azure Container Registry to pull every
	// image kubectl ate installs from, like myregistry.azurecr.io. The
	// registry must hold the images under their usual paths, and the cluster
	// must be attached to it.
	ACR string `json:"acr,omitempty"`
	// NodePool restricts atelet to the nodes of one node pool. WorkerPools
	// then need the same node selector.
	NodePool string `json:"nodePool,omitempty"`
	// ACRCredentialProvider lets atelet pull actor images from ACR with the
	// node's acr-credential-provider. atelet then reads the node's
	// /etc/kubernetes/azure.json and holds the kubelet identity's registry
	// credentials, or a service principal's secret on clusters that use one.
	ACRCredentialProvider bool `json:"acrCredentialProvider,omitempty"`
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
		Profile:     ProfileGeneric,
		Postgres:    Postgres{Type: PostgresBundled},
		ObjectStore: ObjectStore{Type: ObjectStoreBundled},
		Timeout:     metav1.Duration{Duration: 10 * time.Minute},
	}
}

// ReadConfig decodes a config from r and fills unset fields from
// DefaultConfig. dir resolves relative ValuesFiles. A non-empty profile is
// applied with SetProfile.
func ReadConfig(r io.Reader, dir string, profile Profile) (Config, error) {
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
	if err := cfg.SetProfile(profile); err != nil {
		return Config{}, err
	}
	return cfg, cfg.validate()
}

// SetProfile sets the profile unless p is empty. A profile already set to
// anything but generic must equal p.
func (c *Config) SetProfile(p Profile) error {
	if p == "" {
		return nil
	}
	if err := validateProfile(p); err != nil {
		return err
	}
	if c.Profile != ProfileGeneric && c.Profile != p {
		return fmt.Errorf("profile %s conflicts with profile: %s in the config", p, c.Profile)
	}
	c.Profile = p
	return nil
}

// ValidateACR checks that s is empty or an Azure Container Registry login
// server.
func ValidateACR(s string) error {
	if s == "" {
		return nil
	}
	// Without a dot, image references read the name as a Docker Hub namespace.
	if !strings.Contains(s, ".") || strings.ContainsFunc(s, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) {
		return fmt.Errorf("invalid ACR %q: must be a login server like myregistry.azurecr.io", s)
	}
	return nil
}

// ValidateNodePool checks that s is empty or a valid node pool label value.
func ValidateNodePool(s string) error {
	if errs := validation.IsValidLabelValue(s); len(errs) > 0 {
		return fmt.Errorf("invalid node pool %q: %s", s, strings.Join(errs, "; "))
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

func (c Config) validate() error {
	if err := validateProfile(c.Profile); err != nil {
		return err
	}
	if c.AKS != (AKS{}) && c.Profile != ProfileAKS {
		return fmt.Errorf("aks needs profile: %s", ProfileAKS)
	}
	if err := ValidateACR(c.AKS.ACR); err != nil {
		return fmt.Errorf("aks.acr: %w", err)
	}
	if err := ValidateNodePool(c.AKS.NodePool); err != nil {
		return fmt.Errorf("aks.nodePool: %w", err)
	}
	switch c.Postgres.Type {
	case PostgresBundled, PostgresExternal:
	default:
		return fmt.Errorf("invalid postgres.type %q: must be %s or %s", c.Postgres.Type, PostgresBundled, PostgresExternal)
	}
	if err := c.ObjectStore.validate(); err != nil {
		return err
	}
	if c.StorageClass != "" {
		if err := ValidateStorageClass(c.StorageClass); err != nil {
			return fmt.Errorf("storageClass: %w", err)
		}
		if c.Postgres.Type != PostgresBundled && c.ObjectStore.Type != ObjectStoreBundled {
			return fmt.Errorf("storageClass needs postgres.type: %s or objectStore.type: %s", PostgresBundled, ObjectStoreBundled)
		}
	}
	if c.MicroVM.Enabled && c.ObjectStore.Type != ObjectStoreBundled && c.MicroVM.AssetBucket == "" {
		return errors.New("microvm with an s3 or gcs objectStore needs microvm.assetBucket")
	}
	if c.Timeout.Duration <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}

func validateProfile(p Profile) error {
	if !slices.Contains(Profiles, p) {
		return fmt.Errorf("invalid profile %q: must be one of %v", p, Profiles)
	}
	return nil
}

// imageRegistry returns the registry to pull every image kubectl ate installs
// from, or "" to keep each image's own.
func (c Config) imageRegistry() string {
	if c.Profile == ProfileAKS {
		return c.AKS.ACR
	}
	return ""
}

// aksNodePoolLabel is the node label AKS sets to the node's pool name.
const aksNodePoolLabel = "kubernetes.azure.com/agentpool"

// The kubelet's credential provider files on AKS nodes. The provider reads
// the kubelet identity from aksCloudConfig.
const (
	aksCredentialProviderConfig = "/var/lib/kubelet/credential-provider-config.yaml"
	aksCredentialProviderBinDir = "/var/lib/kubelet/credential-provider"
	aksCloudConfig              = "/etc/kubernetes/azure.json"
)

// nodeSelector returns the node selector atelet runs with, or nil for every
// node.
func (c Config) nodeSelector() map[string]string {
	if c.Profile == ProfileAKS && c.AKS.NodePool != "" {
		return map[string]string{aksNodePoolLabel: c.AKS.NodePool}
	}
	return nil
}

// profileValues returns the profile's Helm values for r.
func (c Config) profileValues(r release) map[string]any {
	vals := map[string]any{}
	if reg := c.imageRegistry(); reg != "" {
		vals["global"] = map[string]any{"imageRegistry": reg}
	}
	if r == ateletRelease {
		atelet := map[string]any{}
		if sel := c.nodeSelector(); sel != nil {
			nodeSelector := map[string]any{}
			for k, v := range sel {
				nodeSelector[k] = v
			}
			atelet["nodeSelector"] = nodeSelector
		}
		if c.Profile == ProfileAKS {
			// Off resets the chart defaults, so atelet loses the kubelet
			// identity's registry access that an earlier install gave it.
			config, binDir, hostFiles := "", "", []any{}
			if c.AKS.ACRCredentialProvider {
				config, binDir, hostFiles = aksCredentialProviderConfig, aksCredentialProviderBinDir, []any{aksCloudConfig}
			}
			atelet["imageCredentialProviderConfig"] = config
			atelet["imageCredentialProviderConfigYAML"] = ""
			atelet["imageCredentialProviderBinDir"] = binDir
			atelet["imageCredentialProviderHostFiles"] = hostFiles
		}
		if len(atelet) > 0 {
			vals["atelet"] = atelet
		}
	}
	return vals
}

func (r *Releases) all() []*ReleaseValues {
	return []*ReleaseValues{&r.All, &r.PodCert, &r.Atelet, &r.Substrate}
}

// userValues returns each release's values, keyed by release name: the
// profile's, All's files and values, then the release's own.
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
		user[r.name] = mergeValues(c.profileValues(r), all, own)
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
