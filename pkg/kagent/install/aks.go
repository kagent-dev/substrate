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
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"go.ate.dev/substrate/internal/deviceplugin"
)

// networkPolicyEngines are the DaemonSets of the engines AKS installs with
// az aks create --network-policy. Older clusters run Calico in kube-system.
var networkPolicyEngines = []types.NamespacedName{
	{Namespace: "kube-system", Name: "cilium"},
	{Namespace: "kube-system", Name: "azure-npm"},
	{Namespace: "calico-system", Name: "calico-node"},
	{Namespace: "kube-system", Name: "calico-node"},
}

// checkAKS runs the aks profile's cluster checks. Upgrade runs them too, since
// it applies the profile's values.
func (in *Installer) checkAKS(ctx context.Context, progress func(string)) (string, error) {
	progress("checking NetworkPolicy enforcement")
	engine, err := in.checkNetworkPolicy(ctx)
	if err != nil {
		return "", err
	}
	summary := "NetworkPolicy by " + engine
	if pool := in.opts.AKS.NodePool; pool != "" {
		n, err := in.checkNodePool(ctx)
		if err != nil {
			return "", err
		}
		summary += fmt.Sprintf(", node pool %s with %d nodes", pool, n)
	}
	return summary, nil
}

// checkNetworkPolicy returns the DaemonSet that enforces NetworkPolicy. An AKS
// cluster without one accepts NetworkPolicy objects and ignores them, which
// would leave Substrate's components and WorkerPools unisolated.
func (in *Installer) checkNetworkPolicy(ctx context.Context) (string, error) {
	for _, e := range networkPolicyEngines {
		_, err := in.kube.AppsV1().DaemonSets(e.Namespace).Get(ctx, e.Name, metav1.GetOptions{})
		if err == nil {
			return e.String(), nil
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	return "", errors.New("nothing in the cluster enforces NetworkPolicy; enable it with az aks update --network-policy, or set profile: generic to skip this check")
}

// checkNodePool returns how many nodes aks.nodePool has.
func (in *Installer) checkNodePool(ctx context.Context) (int, error) {
	pool := in.opts.AKS.NodePool
	nodes, err := in.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(in.opts.nodeSelector()).String()})
	if err != nil {
		return 0, err
	}
	if len(nodes.Items) == 0 {
		return 0, fmt.Errorf("node pool %s has no nodes; check aks.nodePool against az aks nodepool list", pool)
	}
	return len(nodes.Items), nil
}

// checkKVM waits for a node running atelet to advertise ate.dev/kvm, which
// microVM workers request. atelet advertises it only on nodes with /dev/kvm.
func (in *Installer) checkKVM(ctx context.Context, progress func(string)) (string, error) {
	sel := labels.SelectorFromSet(in.opts.nodeSelector()).String()
	var total int
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		nodes, err := in.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return false, err
		}
		total = len(nodes.Items)
		for _, n := range nodes.Items {
			if q, ok := n.Status.Allocatable[deviceplugin.ResourceKVM]; ok && !q.IsZero() {
				return true, nil
			}
		}
		progress(fmt.Sprintf("waiting for %s on %d nodes", deviceplugin.ResourceKVM, total))
		return false, nil
	})
	if err != nil {
		return "", fmt.Errorf("no node advertises %s, so microVM workers can't schedule; use a generation 2 VM size with nested virtualization, like Standard_D4s_v5: %w", deviceplugin.ResourceKVM, err)
	}
	return deviceplugin.ResourceKVM + " advertised", nil
}
