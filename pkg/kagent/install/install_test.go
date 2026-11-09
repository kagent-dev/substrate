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
	"io"
	"reflect"
	"slices"
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
		{"", "0.1.0", "0.1.0"},
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
			ch, err := loadChart(chartsfs.FS, r.chart, "v1.2.3")
			if err != nil {
				t.Fatal(err)
			}
			if ch.Metadata.Version != "1.2.3" || ch.Metadata.AppVersion != "v1.2.3" {
				t.Errorf("got version %q app %q, want 1.2.3 v1.2.3", ch.Metadata.Version, ch.Metadata.AppVersion)
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
	in := &Installer{kube: kc, opts: Options{Config: Config{ObjectStore: ObjectStore{Type: ObjectStoreBundled}}}}

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
	s3 := ObjectStore{Type: ObjectStoreS3, Bucket: "b"}
	gcs := ObjectStore{Type: ObjectStoreGCS, Bucket: "b", GCS: GCSObjectStore{TransferServiceAccount: "sa@p.iam.gserviceaccount.com"}}
	tests := []struct {
		name    string
		store   ObjectStore
		user    map[release]map[string]any
		stored  map[release]map[string]any
		secrets []string
		wantErr string
	}{
		{name: "s3", store: s3},
		{name: "gcs", store: gcs},
		{
			name:  "location from chart values",
			store: ObjectStore{Type: ObjectStoreS3},
			user:  map[release]map[string]any{substrateRelease: {"ateApi": map[string]any{"snapshotLocation": "gs://b/"}}},
		},
		{
			name:   "location from the release",
			store:  ObjectStore{Type: ObjectStoreS3},
			stored: map[release]map[string]any{substrateRelease: {"ateApi": map[string]any{"snapshotLocation": "gs://b/"}}},
		},
		{
			name:  "per-template locations",
			store: ObjectStore{Type: ObjectStoreS3},
			user:  map[release]map[string]any{substrateRelease: {"preview": []any{"PerTemplateSnapshotLocation"}}},
		},
		{name: "no location", store: ObjectStore{Type: ObjectStoreS3}, wantErr: "objectStore.bucket"},
		{name: "gcs without service account", store: ObjectStore{Type: ObjectStoreGCS, Bucket: "b"}, wantErr: "objectStore.gcs.transferServiceAccount"},
		{
			name:    "credentials secret present",
			store:   ObjectStore{Type: ObjectStoreS3, Bucket: "b", S3: S3ObjectStore{CredentialsSecret: "s3-creds"}},
			secrets: []string{"s3-creds"},
		},
		{
			name:    "credentials secret missing",
			store:   ObjectStore{Type: ObjectStoreS3, Bucket: "b", S3: S3ObjectStore{CredentialsSecret: "s3-creds"}},
			wantErr: "doesn't exist",
		},
		{
			name:    "credentials secret from chart values",
			store:   s3,
			user:    map[release]map[string]any{ateletRelease: {"objectStore": map[string]any{"credentialsSecret": "s3-creds"}}},
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
			in := &Installer{kube: kc, opts: Options{Config: Config{ObjectStore: tc.store}}, user: map[string]map[string]any{}}
			for r, v := range tc.user {
				in.user[r.name] = v
			}
			atelet, substrate := in.externalStoreValues()
			_, err := in.checkExternalObjectStore(context.Background(),
				mergeValues(tc.stored[substrateRelease], tc.user[substrateRelease], substrate),
				mergeValues(tc.stored[ateletRelease], tc.user[ateletRelease], atelet))
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
	in := &Installer{opts: Options{Config: Config{ObjectStore: ObjectStore{Type: ObjectStoreGCS}, Postgres: Postgres{Type: PostgresExternal}}}}
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

func TestExternalStoreValuesS3(t *testing.T) {
	userEnv := map[string]any{"name": "HTTPS_PROXY", "value": "http://proxy:3128"}
	in := &Installer{
		opts: Options{Config: Config{ObjectStore: ObjectStore{Type: ObjectStoreS3, Bucket: "b", S3: S3ObjectStore{
			Region: "us-east-1", Endpoint: "http://minio:9000", PathStyle: true, CredentialsSecret: "creds",
		}}}},
		user: map[string]map[string]any{substrateRelease.name: {"objectStorePlugin": map[string]any{"extraEnv": []any{userEnv}}}},
	}
	atelet, substrate := in.externalStoreValues()
	wantAtelet := map[string]any{"objectStore": map[string]any{
		"backend": "s3", "region": "us-east-1", "endpoint": "http://minio:9000", "pathStyle": true, "credentialsSecret": "creds",
	}}
	if !reflect.DeepEqual(atelet, wantAtelet) {
		t.Errorf("atelet values = %v, want %v", atelet, wantAtelet)
	}
	secret := func(key string) map[string]any {
		return map[string]any{"name": key, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "creds", "key": key}}}
	}
	wantSubstrate := map[string]any{
		"rustfs": map[string]any{"enabled": false},
		"atelet": map[string]any{"storageBackend": "s3"},
		"ateApi": map[string]any{"snapshotLocation": "gs://b/"},
		"objectStorePlugin": map[string]any{"extraEnv": []any{
			userEnv,
			map[string]any{"name": "AWS_REGION", "value": "us-east-1"},
			map[string]any{"name": "AWS_ENDPOINT_URL", "value": "http://minio:9000"},
			map[string]any{"name": "AWS_S3_USE_PATH_STYLE", "value": "true"},
			secret("AWS_ACCESS_KEY_ID"),
			secret("AWS_SECRET_ACCESS_KEY"),
		}},
	}
	if !reflect.DeepEqual(substrate, wantSubstrate) {
		t.Errorf("substrate values = %v, want %v", substrate, wantSubstrate)
	}
}

func TestInstallStepsDatabase(t *testing.T) {
	tests := []struct {
		postgres  PostgresType
		wantCheck bool
	}{
		{PostgresBundled, false},
		{PostgresExternal, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.postgres), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Postgres.Type = tc.postgres
			in := &Installer{opts: Options{Config: cfg}, report: PlainReporter{W: io.Discard}}
			steps, err := in.InstallSteps()
			if err != nil {
				t.Fatal(err)
			}
			got := slices.ContainsFunc(steps, func(s Step) bool { return s.Name == StepCheckDatabase })
			if got != tc.wantCheck {
				t.Errorf("has %q = %t, want %t", StepCheckDatabase, got, tc.wantCheck)
			}
		})
	}
}
