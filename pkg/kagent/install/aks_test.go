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
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckNetworkPolicy(t *testing.T) {
	ds := func(ns, name string) runtime.Object {
		return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	tests := []struct {
		name    string
		objs    []runtime.Object
		want    string
		wantErr string
	}{
		{name: "cilium", objs: []runtime.Object{ds("kube-system", "cilium")}, want: "kube-system/cilium"},
		{name: "azure-npm", objs: []runtime.Object{ds("kube-system", "azure-npm")}, want: "kube-system/azure-npm"},
		{name: "calico", objs: []runtime.Object{ds("calico-system", "calico-node")}, want: "calico-system/calico-node"},
		{name: "calico in kube-system", objs: []runtime.Object{ds("kube-system", "calico-node")}, want: "kube-system/calico-node"},
		{name: "none", objs: []runtime.Object{ds("kube-system", "kube-proxy")}, wantErr: "az aks update --network-policy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &Installer{kube: fake.NewClientset(tc.objs...)}
			got, err := in.checkNetworkPolicy(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("checkNetworkPolicy() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("checkNetworkPolicy() = %q, want %q", got, tc.want)
			}
		})
	}
}
