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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
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
	if in.opts.AKS.ManagedPrometheus {
		if err := in.checkManagedPrometheus(); err != nil {
			return "", err
		}
		summary += ", Managed Prometheus"
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

// managedPrometheusAPI is the PodMonitor API version of Azure Managed
// Prometheus.
const managedPrometheusAPI = "azmonitoring.coreos.com/v1"

// checkManagedPrometheus checks that the cluster serves the PodMonitors that
// aks.managedPrometheus creates.
func (in *Installer) checkManagedPrometheus() error {
	res, err := in.kube.Discovery().ServerResourcesForGroupVersion(managedPrometheusAPI)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("while discovering %s: %w", managedPrometheusAPI, err)
	}
	if err == nil {
		for _, r := range res.APIResources {
			if r.Name == "podmonitors" {
				return nil
			}
		}
	}
	return errors.New("aks.managedPrometheus needs Azure Managed Prometheus; enable it with az aks update --enable-azure-monitor-metrics")
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
