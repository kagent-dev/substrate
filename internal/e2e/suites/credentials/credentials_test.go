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

package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// TestKubernetesCredentialInjection requires the Helm values in values.yaml and
// an egress MITM CA. It uses real actors, Kubernetes RBAC, and the deployed
// provider. The origin uses the cluster's serving CA to keep all traffic local.
func TestKubernetesCredentialInjection(t *testing.T) {
	if os.Getenv("E2E_CREDENTIAL_PROVIDER") == "" {
		t.Skip("enable the credential-provider Helm E2E values and set E2E_CREDENTIAL_PROVIDER=1")
	}
	env, err := e2e.CheckEnv("BUCKET_NAME", "KO_DOCKER_REPO")
	require.NoError(t, err)
	ctx := t.Context()
	clients := e2e.GetClients()
	namespace, template := e2e.DeployProbe(t, env["BUCKET_NAME"], "credentials", e2e.WithTrustBundle())
	otherNamespace := e2e.CreateNamespace(t).Name
	api, err := clients.K8s.AppsV1().Deployments("ate-system").Get(ctx, "ate-api-server", metav1.GetOptions{})
	require.NoError(t, err)
	serviceAccount := api.Spec.Template.Spec.ServiceAccountName
	require.NotEmpty(t, serviceAccount)

	for _, secret := range []struct{ namespace, name string }{
		{namespace, "allowed"}, {namespace, "no-rbac"}, {otherNamespace, "allowed"},
	} {
		_, err := clients.K8s.CoreV1().Secrets(secret.namespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secret.name},
			Data:       map[string][]byte{"token": []byte("e2e-credential-token")},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	for _, ns := range []string{namespace, otherNamespace} {
		_, err := clients.K8s.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "credential-reader"},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{"allowed"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		_, err = clients.K8s.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "credential-reader"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "credential-reader"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "ate-system", Name: serviceAccount}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	// Prove the negative controls isolate different boundaries: Kubernetes
	// permits the other namespace, but the provider policy does not; within
	// the granted namespace, Kubernetes itself refuses the second Secret.
	for _, tc := range []struct {
		namespace, secret string
		allowed           bool
	}{{namespace, "allowed", true}, {otherNamespace, "allowed", true}, {namespace, "no-rbac", false}} {
		require.Eventually(t, func() bool {
			review, err := clients.K8s.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User:   "system:serviceaccount:ate-system:" + serviceAccount,
					Groups: []string{"system:serviceaccounts", "system:serviceaccounts:ate-system", "system:authenticated"},
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: tc.namespace, Verb: "get", Resource: "secrets", Name: tc.secret,
					},
				},
			}, metav1.CreateOptions{})
			return err == nil && review.Status.Allowed == tc.allowed
		}, 30*time.Second, time.Second, "unexpected Secret RBAC for %s/%s", tc.namespace, tc.secret)
	}

	trustOriginCA(t, clients)
	e2e.DeployServerPod(t, ctx, e2e.ServerPod{
		Name: "credential-origin", Namespace: namespace,
		ImportPath: "github.com/agent-substrate/substrate/internal/e2e/fixtures/testserver",
		Args:       []string{"http", "--tls-bundle=/run/tls/bundle.pem", "--authorization-file=/run/token/token"},
		Port:       443, TargetPort: 8443, HTTPSProbe: true,
		Volumes: []corev1.Volume{
			{Name: "token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "allowed"}}},
			{Name: "tls", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{PodCertificate: &corev1.PodCertificateProjection{
					SignerName: "servicedns.podcert.ate.dev/identity", KeyType: "ECDSAP256", CredentialBundlePath: "bundle.pem",
				}}},
			}}},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "tls", MountPath: "/run/tls", ReadOnly: true}, {Name: "token", MountPath: "/run/token", ReadOnly: true}},
	})
	host := "credential-origin." + namespace + ".svc"
	router, err := e2e.NewRouterClient(ctx)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, tc := range []struct {
		name, secretNamespace, secret, scheme string
		want                                  string
	}{
		{"without-injection", "", "", "https", "401"},
		{"allowed", namespace, "allowed", "https", "204"},
		{"namespace-denied", otherNamespace, "allowed", "https", "403"},
		{"rbac-denied", namespace, "no-rbac", "https", "403"},
		{"cleartext-denied", namespace, "allowed", "http", "403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor := &ateapipb.ObjectRef{Atespace: namespace, Name: tc.name}
			_, _ = clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: actor})
			_, _ = clients.SubstrateAPI.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: actor})
			_, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
				Metadata:      &ateapipb.ResourceMetadata{Atespace: namespace, Name: tc.name},
				ActorTemplate: &ateapipb.ObjectRef{Atespace: namespace, Name: template.GetMetadata().GetName()},
			}})
			require.NoError(t, err)
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_, _ = clients.SubstrateAPI.SuspendActor(cleanupCtx, &ateapipb.SuspendActorRequest{Actor: actor})
				_, err := clients.SubstrateAPI.DeleteActor(cleanupCtx, &ateapipb.DeleteActorRequest{Actor: actor})
				if err != nil {
					t.Errorf("delete actor %s: %v", tc.name, err)
				}
			})
			rule := e2e.EgressAllowHostnames(host)
			if tc.secret != "" {
				rule.Hostnames.Effects = &ateapipb.EgressRuleEffects{InjectStaticHeaders: []*ateapipb.CredentialHeaderInjection{{
					Header: "authorization", Prefix: "Bearer ",
					CredentialUri: fmt.Sprintf("ate-secret://kubernetes.io/%s/%s/token", tc.secretNamespace, tc.secret),
				}}}
			}
			e2e.EnsureEgressPolicy(t, ctx, clients, actor, rule)
			_, err = clients.SubstrateAPI.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: actor})
			require.NoError(t, err)
			path := "/fetch?roots=bundle&url=" + url.QueryEscape(tc.scheme+"://"+host+"/credential")
			// ConfigMap projections and route discovery are asynchronous. Each
			// retry still requires the precise result; transport errors never pass.
			deadline := time.Now().Add(90 * time.Second)
			for {
				resp, err := router.Get(ctx, resources.ActorRef{Atespace: namespace, Name: tc.name}, path)
				var body []byte
				if err == nil {
					body, err = io.ReadAll(resp.Body)
					resp.Body.Close()
					var result struct{ Status, Error string }
					if err == nil && resp.StatusCode == http.StatusOK && json.Unmarshal(body, &result) == nil && result.Error == "" && result.Status == tc.want {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("want origin status %s; last response: %s; error: %v", tc.want, body, err)
				}
				time.Sleep(2 * time.Second)
			}
		})
	}
}

// Only the local origin needs a private CA. Keep the production route, actor
// authentication, and credential-provider TLS settings intact, and restore the
// gateway configuration after the test.
func trustOriginCA(t *testing.T, clients *e2e.Clients) {
	t.Helper()
	configMaps := clients.K8s.CoreV1().ConfigMaps("ate-system")
	config, err := configMaps.Get(t.Context(), "atenet-egress-agentgateway-config", metav1.GetOptions{})
	require.NoError(t, err)
	original := config.Data["config.yaml"]
	require.Equal(t, 1, strings.Count(original, "backendTLS: {}"), "expected one dynamic HTTPS upstream")
	config.Data["config.yaml"] = strings.Replace(original, "backendTLS: {}",
		"backendTLS: {root: /run/servicedns.podcert.ate.dev/trust-bundle.pem}", 1)
	_, err = configMaps.Update(t.Context(), config, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		config, err := configMaps.Get(ctx, config.Name, metav1.GetOptions{})
		if err == nil {
			config.Data["config.yaml"] = original
			_, err = configMaps.Update(ctx, config, metav1.UpdateOptions{})
		}
		if err != nil {
			t.Errorf("restore gateway configuration: %v", err)
		}
	})
}
