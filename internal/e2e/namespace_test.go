// Copyright 2026 Google LLC
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

package e2e

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCopyTokenModeCredentials(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "token-mode-tls", Namespace: "ate-system"}, Data: map[string][]byte{"tls.key": []byte("key")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "token-mode-ca", Namespace: "ate-system"}, Data: map[string]string{"ca.crt": "ca"}},
	)
	copyTokenModeCredentials(t, client.CoreV1(), "worker")

	secret, err := client.CoreV1().Secrets("worker").Get(t.Context(), "token-mode-tls", metav1.GetOptions{})
	if err != nil || string(secret.Data["tls.key"]) != "key" {
		t.Fatalf("copied Secret = %+v, err = %v", secret, err)
	}
	configMap, err := client.CoreV1().ConfigMaps("worker").Get(t.Context(), "token-mode-ca", metav1.GetOptions{})
	if err != nil || configMap.Data["ca.crt"] != "ca" {
		t.Fatalf("copied ConfigMap = %+v, err = %v", configMap, err)
	}
}
