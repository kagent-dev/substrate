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

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/agent-substrate/substrate/pkg/proto/credproviderpb"
)

// TestAgentgatewayInjection exercises the rendered Helm route with a real AGW
// image and provider. Only Kubernetes storage and the ateapi actor lookup are
// faked. Docker must run locally on Linux for host networking.
func TestAgentgatewayInjection(t *testing.T) {
	image := os.Getenv("AGENTGATEWAY_TEST_IMAGE")
	if image == "" {
		t.Skip("set AGENTGATEWAY_TEST_IMAGE to run the Docker integration test")
	}
	require.Equal(t, "linux", runtime.GOOS, "test requires Linux host networking")
	dir := t.TempDir()
	ca, err := localca.GenerateCA("integration", localca.KeyTypeECDSAP256, time.Hour)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(ca.RootCertificate)
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, data, 0600))
		return path
	}
	caPEM, err := ca.TLSCertificateChainPEM()
	require.NoError(t, err)
	caPath := write("ca.pem", caPEM)
	caKey, err := ca.TLSPrivateKeyPEM()
	require.NoError(t, err)
	write("ca-key.pem", caKey)
	servingCert := issueCertificate(t, ca, "")
	writeBundle := func(name string, cert tls.Certificate) string {
		key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		require.NoError(t, err)
		bundle := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})...)
		return write(name, bundle)
	}
	oldBundle, oldCAFile, oldInjector := *serverBundle, *clientCAFile, *injectorSPIFFEID
	t.Cleanup(func() { *serverBundle, *clientCAFile, *injectorSPIFFEID = oldBundle, oldCAFile, oldInjector })
	*serverBundle, *clientCAFile = writeBundle("server.pem", servingCert), caPath
	*injectorSPIFFEID = "spiffe://cluster.local/ns/ate-system/sa/atenet-egress"
	writeBundle("injector.pem", issueCertificate(t, ca, *injectorSPIFFEID))
	creds, err := buildServerCreds(t.Context())
	require.NoError(t, err)
	kube := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "team-a-secrets"},
		Data:       map[string][]byte{"token": []byte("injected-token")},
	})
	rpc := grpc.NewServer(grpc.Creds(creds))
	credproviderpb.RegisterCredentialProviderServer(rpc, NewServer(kube, &namespaceAuthorizer{
		allowed: map[string]map[string]struct{}{"team-a": {"team-a-secrets": {}}},
	}))
	ateapipb.RegisterControlServer(rpc, &credentialTestControl{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = rpc.Serve(lis) }()
	t.Cleanup(rpc.Stop)

	var upstreamCalls atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer injected-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{servingCert}, MinVersion: tls.VersionTLS13}
	origin.StartTLS()
	t.Cleanup(origin.Close)
	_, originPort, err := net.SplitHostPort(origin.Listener.Addr().String())
	require.NoError(t, err)
	target := "localhost:" + originPort

	// Keep the chart's route and TLS policies; substitute only local endpoints
	// and test certificates, including the TLS origin's private CA.
	rendered, err := exec.CommandContext(t.Context(), "helm", "template", "substrate", "../../charts/substrate",
		"-n", "ate-system", "--set", "ateApi.credentialProvider.enabled=true").CombinedOutput()
	require.NoError(t, err, "%s", rendered)
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	var config string
	for {
		var doc struct {
			Kind string
			Data map[string]string
		}
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if doc.Kind == "ConfigMap" && strings.Contains(doc.Data["config.yaml"], "credentialProviders:") {
			config = doc.Data["config.yaml"]
		}
	}
	require.NotEmpty(t, config)
	portReservation, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gatewayAddr := portReservation.Addr().String()
	_, gatewayPort, err := net.SplitHostPort(gatewayAddr)
	require.NoError(t, err)
	require.NoError(t, portReservation.Close())
	config = strings.NewReplacer(
		"api.ate-system.svc:443", lis.Addr().String(),
		"api.ate-system.svc:50051", lis.Addr().String(),
		"port: 8443", "port: "+gatewayPort,
		"/run/servicedns.podcert.ate.dev/credential-bundle.pem", "/config/server.pem",
		"/run/podidentity.podcert.ate.dev/credential-bundle.pem", "/config/injector.pem",
		"/run/servicedns.podcert.ate.dev/trust-bundle.pem", "/config/ca.pem",
		"/run/actor-id-ca-certs/ca.crt", "/config/ca.pem",
		"/run/egress-mitm/tls.crt", "/config/ca.pem",
		"/run/egress-mitm/tls.key", "/config/ca-key.pem",
		"backendTLS: {}", "backendTLS: {root: /config/ca.pem}",
	).Replace(config)
	write("config.yaml", []byte("config:\n  adminAddr: 127.0.0.1:0\n  statsAddr: 127.0.0.1:0\n  readinessAddr: 127.0.0.1:0\n"+config))
	container, err := exec.CommandContext(t.Context(), "docker", "run", "--detach", "--rm", "--network=host",
		"--user=0:0", "--volume", dir+":/config:ro", image, "-f", "/config/config.yaml").CombinedOutput()
	require.NoError(t, err, "%s", container)
	id := strings.TrimSpace(string(container))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if t.Failed() {
			logs, _ := exec.CommandContext(ctx, "docker", "logs", id).CombinedOutput()
			t.Logf("AGW logs:\n%s", logs)
		}
		out, err := exec.CommandContext(ctx, "docker", "stop", "--time=1", id).CombinedOutput()
		if err != nil {
			t.Errorf("stop AGW: %v: %s", err, out)
		}
	})
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", gatewayAddr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 20*time.Second, 100*time.Millisecond, "AGW did not start")

	for _, tc := range []struct {
		name, atespace string
		useTLS         bool
		wantStatus     int
	}{
		{"allowed", "team-a", true, http.StatusNoContent},
		{"different atespace cannot use cached secret", "team-b", true, http.StatusForbidden},
		{"cleartext cannot receive secrets", "team-a", false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeKube, beforeOrigin := len(kube.Actions()), upstreamCalls.Load()
			actorCert := issueCertificate(t, ca, "")
			template, err := x509.ParseCertificate(actorCert.Certificate[0])
			require.NoError(t, err)
			require.NoError(t, substratex509.AddActorIdentityToCertificate(&substratex509.ActorIdentity{
				Atespace: tc.atespace, ActorName: "actor", ActorUid: "uid-1", Purpose: substratex509.ActorIdentityPurposeAtunnel,
			}, template))
			der, err := x509.CreateCertificate(rand.Reader, template, ca.RootCertificate,
				actorCert.PrivateKey.(crypto.Signer).Public(), ca.SigningKey)
			require.NoError(t, err)
			actorCert.Certificate = [][]byte{der}
			outer, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", gatewayAddr, &tls.Config{
				RootCAs: roots, Certificates: []tls.Certificate{actorCert}, MinVersion: tls.VersionTLS13,
			})
			require.NoError(t, err)
			defer outer.Close()
			require.NoError(t, outer.SetDeadline(time.Now().Add(10*time.Second)))
			_, err = fmt.Fprintf(outer, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
			require.NoError(t, err)
			response, err := http.ReadResponse(bufio.NewReader(outer), &http.Request{Method: http.MethodConnect})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, "actor CONNECT authorization")
			var tunnel net.Conn = outer
			if tc.useTLS {
				tunnel = tls.Client(outer, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13})
			}
			_, err = fmt.Fprintf(tunnel, "GET / HTTP/1.1\r\nHost: %s\r\nAuthorization: actor-supplied\r\nConnection: close\r\n\r\n", target)
			require.NoError(t, err)
			response, err = http.ReadResponse(bufio.NewReader(tunnel), &http.Request{Method: http.MethodGet})
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, tc.wantStatus, response.StatusCode)
			if tc.wantStatus == http.StatusNoContent {
				require.Len(t, kube.Actions(), beforeKube+1)
				require.Equal(t, beforeOrigin+1, upstreamCalls.Load())
			} else {
				require.Len(t, kube.Actions(), beforeKube, "denied request must not read Kubernetes")
				require.Equal(t, beforeOrigin, upstreamCalls.Load(), "denied request must not reach upstream")
			}
		})
	}
}

type credentialTestControl struct {
	ateapipb.UnimplementedControlServer
}

func (*credentialTestControl) GetActor(_ context.Context, _ *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	return &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Uid: "uid-1"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}, nil
}

func (*credentialTestControl) GetActorEgressPolicy(_ context.Context, _ *ateapipb.GetActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	return &ateapipb.EgressPolicy{Rules: []*ateapipb.EgressRule{{Hostnames: &ateapipb.HostnameRule{
		Patterns: []string{"localhost"},
		Effects: &ateapipb.EgressRuleEffects{InjectStaticHeaders: []*ateapipb.CredentialHeaderInjection{{
			Header: "authorization", Prefix: "Bearer ", CredentialUri: "ate-secret://kubernetes.io/team-a-secrets/api/token",
		}}},
	}}}}, nil
}
