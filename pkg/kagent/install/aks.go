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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
