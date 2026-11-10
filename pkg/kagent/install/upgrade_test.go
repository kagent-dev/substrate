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
	"strings"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	chartsfs "go.ate.dev/substrate/charts"
)

func TestCheckSkew(t *testing.T) {
	tests := []struct {
		from, to string
		wantErr  string
	}{
		{"1.2.0", "1.2.0", ""},
		{"1.2.0", "1.2.3", ""},
		{"1.2.3", "1.3.0", ""},
		{"1.3.0-rc.1", "1.3.0", ""},
		{"0.1.0", "0.1.1", ""},
		{"1.2.3", "1.2.2", "downgrading"},
		{"1.3.0", "1.2.9", "downgrading"},
		{"1.2.0", "1.4.0", "upgrade 1.2.0 to 1.3 first"},
		{"0.1.0", "0.2.0", "needs a reinstall"},
		{"0.9.0", "1.0.0", "needs a reinstall"},
		{"1.9.0", "2.0.0", "major version"},
		{"dev", "1.0.0", "installed version"},
	}
	for _, tc := range tests {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			err := checkSkew(tc.from, tc.to)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkSkew() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkSkew() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckCRDStorage(t *testing.T) {
	tests := []struct {
		name    string
		stored  []string
		wantErr bool
	}{
		{name: "not installed"},
		{name: "stored version kept", stored: []string{"v1"}},
		{name: "stored version dropped", stored: []string{"v1alpha1", "v1"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ac := apiextfake.NewClientset()
			if tc.stored != nil {
				ac = apiextfake.NewClientset(&apiextv1.CustomResourceDefinition{
					ObjectMeta: metav1.ObjectMeta{Name: "workerpools.ate.dev"},
					Status:     apiextv1.CustomResourceDefinitionStatus{StoredVersions: tc.stored},
				})
			}
			in := &Installer{apiext: ac, charts: chartsfs.FS}
			_, err := in.checkCRDStorage(context.Background(), func(string) {})
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("checkCRDStorage() = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

func TestWriteDiff(t *testing.T) {
	var buf bytes.Buffer
	n, err := writeDiff(&buf, "substrate", "a: 1\nb: 2\n", "a: 1\nb: 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("changed lines = %d, want 2", n)
	}
	if !strings.Contains(buf.String(), "+b: 3") {
		t.Errorf("diff missing added line:\n%s", buf.String())
	}

	buf.Reset()
	if n, _ := writeDiff(&buf, "substrate", "a: 1\n", "a: 1\n"); n != 0 || buf.Len() != 0 {
		t.Errorf("identical manifests: n = %d, output %q", n, buf.String())
	}
}
