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
	"context"
	"reflect"
	"strings"
	"testing"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	chartsfs "go.ate.dev/substrate/charts"
)

func TestMergeValues(t *testing.T) {
	tests := []struct {
		name   string
		layers []map[string]any
		want   map[string]any
	}{
		{
			name: "later layer wins",
			layers: []map[string]any{
				{"a": 1, "b": 1},
				{"b": 2},
			},
			want: map[string]any{"a": 1, "b": 2},
		},
		{
			name: "nested maps merge",
			layers: []map[string]any{
				{"objectStore": map[string]any{"endpoint": "default", "region": "us-east-1"}},
				{"objectStore": map[string]any{"endpoint": "user"}},
			},
			want: map[string]any{"objectStore": map[string]any{"endpoint": "user", "region": "us-east-1"}},
		},
		{
			name: "forced scalar replaces user map",
			layers: []map[string]any{
				nil,
				{"atelet": map[string]any{"enabled": true, "extraArgs": []any{"x"}}},
				{"atelet": map[string]any{"enabled": false}},
			},
			want: map[string]any{"atelet": map[string]any{"enabled": false, "extraArgs": []any{"x"}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeValues(tc.layers...); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mergeValues() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeValuesKeepsInputs(t *testing.T) {
	user := map[string]any{"objectStore": map[string]any{"endpoint": "user"}}
	mergeValues(user, map[string]any{"objectStore": map[string]any{"endpoint": "forced"}})
	if got := user["objectStore"].(map[string]any)["endpoint"]; got != "user" {
		t.Errorf("input modified: endpoint = %v", got)
	}
}

func TestStampVersion(t *testing.T) {
	tests := []struct {
		tag         string
		wantVersion string
		wantApp     string
	}{
		{"v1.2.3", "1.2.3", "v1.2.3"},
		{"v1.2.3-rc.1", "1.2.3-rc.1", "v1.2.3-rc.1"},
		{"dev", "0.1.0", "0.1.0"},
		{"v1.2.3-4-gabcdef-dirty+meta", "0.1.0", "0.1.0"},
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			ch := &chart.Chart{Metadata: &chart.Metadata{Version: "0.1.0", AppVersion: "0.1.0"}}
			stampVersion(ch, tc.tag)
			if ch.Metadata.Version != tc.wantVersion || ch.Metadata.AppVersion != tc.wantApp {
				t.Errorf("got version %q app %q, want %q %q", ch.Metadata.Version, ch.Metadata.AppVersion, tc.wantVersion, tc.wantApp)
			}
		})
	}
}

func TestLoadEmbeddedCharts(t *testing.T) {
	for _, r := range allReleases {
		t.Run(r.chart, func(t *testing.T) {
			ch, err := loadChart(chartsfs.FS, r.chart)
			if err != nil {
				t.Fatal(err)
			}
			if ch.Name() != r.chart {
				t.Errorf("chart name = %q, want %q", ch.Name(), r.chart)
			}
			if len(ch.Templates) == 0 {
				t.Error("chart has no templates")
			}
		})
	}
}

func TestBootstrapKeepsExistingSecrets(t *testing.T) {
	ctx := context.Background()
	kc := fake.NewClientset()
	in := &Installer{kube: kc, opts: Options{ObjectStore: ObjectStoreRustFS}}

	for _, s := range []func(context.Context, func(string)) (string, error){
		in.createPodCertCAs, in.createIdentitySecrets, in.configureObjectStore,
	} {
		if _, err := s(ctx, func(string) {}); err != nil {
			t.Fatal(err)
		}
	}
	before := secretData(t, kc)

	for _, s := range []func(context.Context, func(string)) (string, error){
		in.createPodCertCAs, in.createIdentitySecrets, in.configureObjectStore,
	} {
		msg, err := s(ctx, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		if msg != "already present" {
			t.Errorf("second run = %q, want already present", msg)
		}
	}
	if after := secretData(t, kc); !reflect.DeepEqual(before, after) {
		t.Error("second run changed secrets")
	}

	roots, err := poolRootsPEM(ctx, kc, actorIDCAPool)
	if err != nil {
		t.Fatal(err)
	}
	certs, err := kc.CoreV1().Secrets(SystemNamespace).Get(ctx, "actor-id-ca-certs", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(certs.Data["ca.crt"], roots) {
		t.Error("actor-id-ca-certs doesn't hold actor-id-ca-pool's roots")
	}
}

// secretData returns every Secret's contents keyed by namespace/name.
func secretData(t *testing.T, kc *fake.Clientset) map[string]any {
	t.Helper()
	list, err := kc.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, s := range list.Items {
		out[s.Namespace+"/"+s.Name] = []any{s.Data, s.StringData}
	}
	if len(out) != 8 {
		t.Fatalf("got %d secrets, want 8", len(out))
	}
	return out
}

func TestApplyPostgresManifest(t *testing.T) {
	ctx := context.Background()
	manifest, err := postgresManifest()
	if err != nil {
		t.Fatal(err)
	}
	kc := fake.NewClientset()
	// Applying twice checks the manifest stays appliable over itself.
	for range 2 {
		if err := applyManifest(ctx, kc, manifest); err != nil {
			t.Fatal(err)
		}
	}
	ss, err := kc.AppsV1().StatefulSets(SystemNamespace).Get(ctx, postgresStatefulSet, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The kind overlay sizes the volume for evaluation.
	if got := ss.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String(); got != "1Gi" {
		t.Errorf("volume size = %s, want 1Gi", got)
	}
	if _, err := kc.CoreV1().Services(SystemNamespace).Get(ctx, "postgres", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckExternalObjectStore(t *testing.T) {
	gcs := map[string]any{
		"ateApi":            map[string]any{"snapshotLocation": "gs://b/"},
		"objectStorePlugin": map[string]any{"transferServiceAccount": "sa@p.iam.gserviceaccount.com"},
	}
	tests := []struct {
		name    string
		store   ObjectStore
		user    map[string]any
		secrets []string
		wantErr string
	}{
		{name: "s3", store: ObjectStoreS3, user: map[string]any{"ateApi": map[string]any{"snapshotLocation": "gs://b/"}}},
		{name: "per-template locations", store: ObjectStoreS3, user: map[string]any{"preview": []any{"PerTemplateSnapshotLocation"}}},
		{name: "no location", store: ObjectStoreS3, wantErr: "ateApi.snapshotLocation"},
		{name: "gcs", store: ObjectStoreGCS, user: gcs},
		{name: "gcs without service account", store: ObjectStoreGCS, user: map[string]any{"ateApi": map[string]any{"snapshotLocation": "gs://b/"}}, wantErr: "transferServiceAccount"},
		{
			name:    "credentials secret present",
			store:   ObjectStoreS3,
			user:    map[string]any{"preview": []any{"ALL"}, "objectStore": map[string]any{"credentialsSecret": "s3-creds"}},
			secrets: []string{"s3-creds"},
		},
		{
			name:    "credentials secret missing",
			store:   ObjectStoreS3,
			user:    map[string]any{"preview": []any{"ALL"}, "objectStore": map[string]any{"credentialsSecret": "s3-creds"}},
			wantErr: "doesn't exist",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewClientset()
			for _, name := range tc.secrets {
				if _, err := kc.CoreV1().Secrets(SystemNamespace).Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			in := &Installer{kube: kc, opts: Options{ObjectStore: tc.store}}
			_, err := in.checkExternalObjectStore(context.Background(), tc.user)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkExternalObjectStore() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkExternalObjectStore() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestExternalObjectStoreValues(t *testing.T) {
	in := &Installer{opts: Options{ObjectStore: ObjectStoreGCS, Postgres: PostgresExternal}}
	defaults, forced, err := in.ateletValues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if defaults != nil {
		t.Errorf("atelet defaults = %v, want none", defaults)
	}
	if got := forced["objectStore"].(map[string]any)["backend"]; got != "gcs" {
		t.Errorf("atelet objectStore.backend = %v, want gcs", got)
	}
	_, forced, err = in.substrateValues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := forced["rustfs"].(map[string]any)["enabled"]; got != false {
		t.Errorf("substrate rustfs.enabled = %v, want false", got)
	}
}
