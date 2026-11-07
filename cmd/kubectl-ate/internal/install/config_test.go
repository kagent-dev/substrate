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
		want    Config
		wantErr string
	}{
		{name: "defaults", in: "{}", want: DefaultConfig()},
		{
			name: "full",
			in: `postgres: external
objectStore: gcs
microvm: {enabled: true, assetBucket: assets}
timeout: 5m
valuesFiles: [gke.yaml, /abs.yaml]
values: {image: {tag: v1}}
`,
			want: withDefaults(func(c *Config) {
				c.Postgres, c.ObjectStore = PostgresExternal, ObjectStoreGCS
				c.MicroVM = MicroVM{Enabled: true, AssetBucket: "assets"}
				c.Timeout = metav1.Duration{Duration: 5 * time.Minute}
				c.ValuesFiles = []string{"/cfg/gke.yaml", "/abs.yaml"}
				c.Values = map[string]any{"image": map[string]any{"tag": "v1"}}
			}),
		},
		{name: "empty", in: "", wantErr: "config is empty"},
		{name: "unknown field", in: "postgress: external\n", wantErr: "postgress"},
		{name: "bad postgres", in: "postgres: cloud\n", wantErr: "invalid postgres"},
		{name: "bad object store", in: "objectStore: nfs\n", wantErr: "invalid objectStore"},
		{name: "microvm needs bucket", in: "objectStore: s3\nmicrovm: {enabled: true}\n", wantErr: "microvm.assetBucket"},
		{name: "bad timeout", in: "timeout: 0s\n", wantErr: "timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadConfig(strings.NewReader(tc.in), "/cfg")
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
	f := filepath.Join(dir, "v.yaml")
	if err := os.WriteFile(f, []byte("image: {registry: r, tag: file}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{ValuesFiles: []string{f}, Values: map[string]any{"image": map[string]any{"tag": "inline"}}}
	got, err := c.userValues()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"image": map[string]any{"registry": "r", "tag": "inline"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("userValues() mismatch (-want +got):\n%s", diff)
	}
}
