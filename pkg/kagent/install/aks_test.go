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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"go.ate.dev/substrate/internal/deviceplugin"
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

func TestCheckManagedPrometheus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources []*metav1.APIResourceList
		wantErr   bool
	}{
		{name: "not enabled", wantErr: true},
		{name: "no podmonitors", resources: []*metav1.APIResourceList{{GroupVersion: managedPrometheusAPI, APIResources: []metav1.APIResource{{Name: "servicemonitors"}}}}, wantErr: true},
		{name: "upstream podmonitors", resources: []*metav1.APIResourceList{{GroupVersion: "monitoring.coreos.com/v1", APIResources: []metav1.APIResource{{Name: "podmonitors"}}}}, wantErr: true},
		{name: "enabled", resources: []*metav1.APIResourceList{{GroupVersion: managedPrometheusAPI, APIResources: []metav1.APIResource{{Name: "podmonitors"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewClientset()
			kc.Resources = tc.resources
			err := (&Installer{kube: kc}).checkManagedPrometheus()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--enable-azure-monitor-metrics") {
					t.Errorf("checkManagedPrometheus() = %v, want an error naming --enable-azure-monitor-metrics", err)
				}
			} else if err != nil {
				t.Errorf("checkManagedPrometheus() = %v", err)
			}
		})
	}
}

func node(name, pool string, kvm bool) runtime.Object {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{aksNodePoolLabel: pool}}}
	if kvm {
		n.Status.Allocatable = corev1.ResourceList{deviceplugin.ResourceKVM: resource.MustParse("4096")}
	}
	return n
}

func TestCheckNodePool(t *testing.T) {
	kc := fake.NewClientset(node("a", "agents", false), node("b", "agents", false), node("c", "system", false))
	for _, tc := range []struct {
		pool    string
		want    int
		wantErr string
	}{
		{pool: "agents", want: 2},
		{pool: "gpu", wantErr: "node pool gpu has no nodes"},
	} {
		in := &Installer{kube: kc, opts: Options{Config: Config{Profile: ProfileAKS, AKS: AKS{NodePool: tc.pool}}}}
		got, err := in.checkNodePool(context.Background())
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("pool %s: checkNodePool() error = %v, want it to contain %q", tc.pool, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("pool %s: checkNodePool() = %d, %v, want %d", tc.pool, got, err, tc.want)
		}
	}
}

func TestCheckKVM(t *testing.T) {
	tests := []struct {
		name    string
		pool    string
		nodes   []runtime.Object
		wantErr bool
	}{
		{name: "any node", nodes: []runtime.Object{node("a", "system", false), node("b", "agents", true)}},
		{name: "in pool", pool: "agents", nodes: []runtime.Object{node("a", "agents", true)}},
		{name: "outside pool", pool: "agents", nodes: []runtime.Object{node("a", "agents", false), node("b", "system", true)}, wantErr: true},
		{name: "none", nodes: []runtime.Object{node("a", "system", false)}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &Installer{kube: fake.NewClientset(tc.nodes...), opts: Options{Config: Config{Profile: ProfileAKS, AKS: AKS{NodePool: tc.pool}}}}
			// The first check runs before the poll sees the canceled context.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := in.checkKVM(ctx, func(string) {})
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("checkKVM() error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}
