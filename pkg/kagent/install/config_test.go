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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReadConfig(t *testing.T) {
	withDefaults := func(f func(*Config)) Config {
		c := DefaultConfig()
		f(&c)
		return c
	}
	tests := []struct {
		name    string
		in      string
		profile string
		want    Config
		wantErr string
	}{
		{name: "defaults", in: "{}", want: DefaultConfig()},
		{
			name: "full",
			in: `postgres: external
objectStore:
  type: gcs
  bucket: snapshots
  gcs: {transferServiceAccount: sa@p.iam.gserviceaccount.com}
microvm: {enabled: true, assetBucket: assets}
timeout: 5m
releases:
  all: {valuesFiles: [gke.yaml, /abs.yaml], values: {image: {tag: v1}}}
  substrate: {valuesFiles: [db.yaml]}
`,
			want: withDefaults(func(c *Config) {
				c.Postgres = Postgres{Type: PostgresExternal}
				c.ObjectStore = ObjectStore{
					Type:   ObjectStoreGCS,
					Bucket: "snapshots",
					GCS:    GCSObjectStore{TransferServiceAccount: "sa@p.iam.gserviceaccount.com"},
				}
				c.MicroVM = MicroVM{Enabled: true, AssetBucket: "assets"}
				c.Timeout = metav1.Duration{Duration: 5 * time.Minute}
				c.Releases.All = ReleaseValues{
					ValuesFiles: []string{"/cfg/gke.yaml", "/abs.yaml"},
					Values:      map[string]any{"image": map[string]any{"tag": "v1"}},
				}
				c.Releases.Substrate.ValuesFiles = []string{"/cfg/db.yaml"}
			}),
		},
		{name: "storage class", in: "storageClass: fast\n", want: withDefaults(func(c *Config) { c.StorageClass = "fast" })},
		{name: "bad storage class", in: "storageClass: Premium_SSD\n", wantErr: "invalid StorageClass name"},
		{name: "profile", in: "{}", profile: "aks", want: withDefaults(func(c *Config) { c.StorageClass = "managed-csi-premium" })},
		{name: "config wins over the profile", in: "storageClass: fast\n", profile: "aks", want: withDefaults(func(c *Config) { c.StorageClass = "fast" })},
		{
			name:    "profile without bundled volumes",
			in:      "postgres: external\nobjectStore: s3\n",
			profile: "aks",
			want: withDefaults(func(c *Config) {
				c.Postgres, c.ObjectStore = Postgres{Type: PostgresExternal}, ObjectStore{Type: ObjectStoreS3}
				c.StorageClass = "managed-csi-premium"
			}),
		},
		{name: "empty", in: "", wantErr: "config is empty"},
		{name: "bad profile", in: "{}", profile: "eks", wantErr: "invalid profile"},
		{name: "profile in the config", in: "profile: aks\n", wantErr: "profile"},
		{name: "image registry", in: "imageRegistry: r.azurecr.io\n", want: withDefaults(func(c *Config) { c.ImageRegistry = "r.azurecr.io" })},
		{name: "image registry with port", in: "imageRegistry: localhost:5001\n", want: withDefaults(func(c *Config) { c.ImageRegistry = "localhost:5001" })},
		{name: "image registry with path", in: "imageRegistry: r.azurecr.io/substrate\n", wantErr: "invalid imageRegistry"},
		{name: "image registry without domain", in: "imageRegistry: myregistry\n", wantErr: "invalid imageRegistry"},
		{name: "image registry with space", in: "imageRegistry: 'r.azurecr.io '\n", wantErr: "invalid imageRegistry"},
		{name: "otel endpoint", in: "otelEndpoint: http://collector:4317\n", want: withDefaults(func(c *Config) { c.OTelEndpoint = "http://collector:4317" })},
		{name: "otel endpoint without scheme", in: "otelEndpoint: collector:4317\n", wantErr: "invalid OTLP endpoint"},
		{name: "unknown field", in: "postgress: external\n", wantErr: "postgress"},
		{name: "unknown release", in: "releases: {crds: {values: {a: 1}}}\n", wantErr: "crds"},
		{name: "bad postgres", in: "postgres: cloud\n", wantErr: "invalid postgres"},
		{
			name: "object store as a string",
			in:   "objectStore: s3\n",
			want: withDefaults(func(c *Config) { c.ObjectStore = ObjectStore{Type: ObjectStoreS3} }),
		},
		{
			name: "s3 object store",
			in:   "objectStore: {type: s3, bucket: b, s3: {region: us-east-1, endpoint: 'http://minio:9000', pathStyle: true, credentialsSecret: creds}}\n",
			want: withDefaults(func(c *Config) {
				c.ObjectStore = ObjectStore{Type: ObjectStoreS3, Bucket: "b", S3: S3ObjectStore{
					Region: "us-east-1", Endpoint: "http://minio:9000", PathStyle: true, CredentialsSecret: "creds",
				}}
			}),
		},
		{
			name: "postgres as a struct",
			in:   "postgres: {type: external}\n",
			want: withDefaults(func(c *Config) { c.Postgres = Postgres{Type: PostgresExternal} }),
		},
		{name: "rustfs alias", in: "objectStore: rustfs\n", want: DefaultConfig()},
		{name: "rustfs alias in a struct", in: "objectStore: {type: rustfs}\n", want: DefaultConfig()},
		{name: "bad object store", in: "objectStore: nfs\n", wantErr: "invalid objectStore"},
		{name: "unknown postgres field", in: "postgres: {type: external, host: db}\n", wantErr: "host"},
		{name: "bundled with bucket", in: "objectStore: {bucket: b}\n", wantErr: "objectStore"},
		{name: "bucket with path", in: "objectStore: {type: s3, bucket: b/dir}\n", wantErr: "objectStore.bucket"},
		{name: "gcs block on s3", in: "objectStore: {type: s3, gcs: {transferServiceAccount: sa}}\n", wantErr: "objectStore.gcs"},
		{name: "s3 endpoint without scheme", in: "objectStore: {type: s3, s3: {endpoint: minio:9000}}\n", wantErr: "objectStore.s3.endpoint"},
		{name: "unknown object store field", in: "objectStore: {type: s3, buckets: b}\n", wantErr: "buckets"},
		{name: "microvm needs bucket", in: "objectStore: s3\nmicrovm: {enabled: true}\n", wantErr: "microvm.assetBucket"},
		{name: "bad timeout", in: "timeout: 0s\n", wantErr: "timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadConfig(strings.NewReader(tc.in), "/cfg", tc.profile)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ReadConfig() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ReadConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestConfigUserValues(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	img := func(registry, tag string) map[string]any {
		return map[string]any{"image": map[string]any{"registry": registry, "tag": tag}}
	}
	c := Config{Releases: Releases{
		All: ReleaseValues{
			ValuesFiles: []string{write("all.yaml", "image: {registry: r, tag: all-file}\n")},
			Values:      map[string]any{"image": map[string]any{"tag": "all"}},
		},
		Atelet: ReleaseValues{ValuesFiles: []string{write("atelet.yaml", "image: {tag: atelet-file}\n")}},
		Substrate: ReleaseValues{
			ValuesFiles: []string{write("substrate.yaml", "image: {registry: s, tag: substrate-file}\n")},
			Values:      map[string]any{"image": map[string]any{"tag": "substrate"}},
		},
	}}
	got, err := c.userValues()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]any{
		"substrate-podcert": img("r", "all"),
		"substrate-atelet":  img("r", "atelet-file"),
		"substrate":         img("s", "substrate"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("userValues() mismatch (-want +got):\n%s", diff)
	}
}

func TestProfileConfig(t *testing.T) {
	for _, p := range append(Profiles(), "") {
		if _, err := ProfileConfig(p); err != nil {
			t.Errorf("ProfileConfig(%q): %v", p, err)
		}
	}
	if _, err := ProfileConfig("eks"); err == nil || !strings.Contains(err.Error(), "invalid profile") {
		t.Errorf("ProfileConfig(eks) error = %v, want invalid profile", err)
	}
}

func TestConfigUserValuesRegistryAndOTel(t *testing.T) {
	registry := func(r string) map[string]any {
		return map[string]any{"global": map[string]any{"imageRegistry": r}}
	}
	otel := func(endpoint string) map[string]any {
		return map[string]any{"endpoint": endpoint}
	}
	substrateValues := registry("mirror.example.com")
	substrateValues["otel"] = otel("http://other:4317")
	c := Config{
		ImageRegistry: "r.azurecr.io",
		OTelEndpoint:  "http://collector:4317",
		Releases:      Releases{Substrate: ReleaseValues{Values: substrateValues}},
	}
	got, err := c.userValues()
	if err != nil {
		t.Fatal(err)
	}
	// The release's own values win, and podcert gets no otel.
	atelet := registry("r.azurecr.io")
	atelet["otel"] = otel("http://collector:4317")
	want := map[string]map[string]any{
		"substrate-podcert": registry("r.azurecr.io"),
		"substrate-atelet":  atelet,
		"substrate":         substrateValues,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("userValues() mismatch (-want +got):\n%s", diff)
	}
}
