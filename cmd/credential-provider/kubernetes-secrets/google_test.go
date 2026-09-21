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
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agent-substrate/substrate/pkg/proto/credproviderpb"
)

const (
	googleTestActor = "spiffe://substrate-actor.local/atespace/team-a/actor/vertex"
	googleTestEmail = "vertex@project.iam.gserviceaccount.com"
	googleTestURI   = "ate-secret://google-access-token.kubernetes.io/ns1/vertex/credentials.json"
)

// fakeTokenEndpoint stands in for Google's token endpoint: it verifies each
// assertion against the service account's public key and records its claims.
type fakeTokenEndpoint struct {
	*httptest.Server
	public    *rsa.PublicKey
	hits      atomic.Int32
	expiresIn int
	// respond, when set, replaces the success response.
	respond func(w http.ResponseWriter, hit int32)

	mu     sync.Mutex
	claims []*assertionClaims
}

// assertionClaims are the JWT bearer claims the exchange is expected to sign.
type assertionClaims struct {
	Iss   string `json:"iss"`
	Scope string `json:"scope"`
	Aud   string `json:"aud"`
}

// verifyAssertion checks an RS256 JWT assertion against the service account's
// public key and returns its claims.
func verifyAssertion(assertion string, public *rsa.PublicKey) (*assertionClaims, error) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed assertion")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		return nil, err
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims assertionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

func newFakeTokenEndpoint(t *testing.T, public *rsa.PublicKey) *fakeTokenEndpoint {
	t.Helper()
	f := &fakeTokenEndpoint{public: public, expiresIn: 3600}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := f.hits.Add(1)
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		claims, err := verifyAssertion(r.PostForm.Get("assertion"), f.public)
		if err != nil {
			http.Error(w, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.claims = append(f.claims, claims)
		f.mu.Unlock()
		if f.respond != nil {
			f.respond(w, hit)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"ya29.token-%d","token_type":"Bearer","expires_in":%d}`, hit, f.expiresIn)
	}))
	t.Cleanup(f.Close)
	return f
}

func generateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// serviceAccountKeyJSON renders a key file of the shape Google issues.
func serviceAccountKeyJSON(t *testing.T, key *rsa.PrivateKey, email, tokenURL string) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "project",
		"private_key_id": "key-1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   email,
		"token_uri":      tokenURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func vertexSecret(key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vertex", Namespace: "ns1"},
		Data:       map[string][]byte{"credentials.json": key},
	}
}

// newGoogleTestServer grants team-a its namespace and points the exchanger at
// the fake endpoint's TLS client.
func newGoogleTestServer(endpoint *fakeTokenEndpoint, secrets ...*corev1.Secret) (*Server, *fake.Clientset) {
	objects := make([]runtime.Object, 0, len(secrets))
	for _, secret := range secrets {
		objects = append(objects, secret)
	}
	client := fake.NewSimpleClientset(objects...)
	srv := NewServer(client, &NamespaceAuthorizer{allowed: map[string]map[string]struct{}{"team-a": {"ns1": {}}}})
	srv.google = newGoogleTokenExchanger(endpoint.Client())
	return srv, client
}

func fetchGoogleToken(t *testing.T, srv *Server, uri string) (string, error) {
	t.Helper()
	resp, err := srv.FetchSecret(t.Context(), &credproviderpb.FetchSecretRequest{Uri: uri, ActorSpiffeId: googleTestActor})
	if err != nil {
		return "", err
	}
	return string(resp.GetOpaqueBytes()), nil
}

func TestFetchSecretGoogleAccessToken(t *testing.T) {
	key := generateRSAKey(t)
	endpoint := newFakeTokenEndpoint(t, &key.PublicKey)
	raw := serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)
	srv, _ := newGoogleTestServer(endpoint, vertexSecret(raw))

	token, err := fetchGoogleToken(t, srv, googleTestURI)
	if err != nil || token != "ya29.token-1" {
		t.Fatalf("FetchSecret = %q, %v", token, err)
	}
	if strings.Contains(token, "PRIVATE KEY") {
		t.Fatal("response carries the private key")
	}
	again, err := fetchGoogleToken(t, srv, googleTestURI)
	if err != nil || again != token {
		t.Fatalf("second FetchSecret = %q, %v, want the cached token", again, err)
	}
	withoutKey, err := fetchGoogleToken(t, srv, "ate-secret://google-access-token.kubernetes.io/ns1/vertex")
	if err != nil || withoutKey != token {
		t.Fatalf("FetchSecret without key = %q, %v", withoutKey, err)
	}
	if hits := endpoint.hits.Load(); hits != 1 {
		t.Fatalf("token endpoint hits = %d, want one exchange for three fetches", hits)
	}
	endpoint.mu.Lock()
	claims := endpoint.claims[0]
	endpoint.mu.Unlock()
	if claims.Iss != googleTestEmail || claims.Scope != googleCloudPlatformScope || claims.Aud != endpoint.URL {
		t.Fatalf("assertion claims = %+v", claims)
	}

	// The plain provider name still returns the Secret entry itself.
	rawValue, err := fetchGoogleToken(t, srv, "ate-secret://kubernetes.io/ns1/vertex/credentials.json")
	if err != nil || rawValue != string(raw) {
		t.Fatalf("kubernetes.io FetchSecret = %q, %v", rawValue, err)
	}
}

func TestGoogleAccessTokenRefreshesBeforeGatewayCacheOutlivesIt(t *testing.T) {
	key := generateRSAKey(t)
	endpoint := newFakeTokenEndpoint(t, &key.PublicKey)
	srv, _ := newGoogleTestServer(endpoint, vertexSecret(serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)))
	now := time.Now()
	srv.google.now = func() time.Time { return now }

	first, err := fetchGoogleToken(t, srv, googleTestURI)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if cached, err := fetchGoogleToken(t, srv, googleTestURI); err != nil || cached != first {
		t.Fatalf("fetch at 30m = %q, %v, want cached %q", cached, err, first)
	}
	now = now.Add(20 * time.Minute)
	refreshed, err := fetchGoogleToken(t, srv, googleTestURI)
	if err != nil || refreshed != "ya29.token-2" {
		t.Fatalf("fetch at 50m = %q, %v, want a fresh token", refreshed, err)
	}
	if hits := endpoint.hits.Load(); hits != 2 {
		t.Fatalf("token endpoint hits = %d, want 2", hits)
	}
}

func TestGoogleAccessTokenSerializesConcurrentFetches(t *testing.T) {
	key := generateRSAKey(t)
	endpoint := newFakeTokenEndpoint(t, &key.PublicKey)
	endpoint.respond = func(w http.ResponseWriter, hit int32) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `{"access_token":"ya29.token-%d","token_type":"Bearer","expires_in":3600}`, hit)
	}
	srv, _ := newGoogleTestServer(endpoint, vertexSecret(serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)))

	const fetches = 16
	tokens := make([]string, fetches)
	errs := make([]error, fetches)
	var wg sync.WaitGroup
	for i := range fetches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens[i], errs[i] = fetchGoogleToken(t, srv, googleTestURI)
		}()
	}
	wg.Wait()
	for i := range fetches {
		if errs[i] != nil || tokens[i] != "ya29.token-1" {
			t.Fatalf("fetch %d = %q, %v", i, tokens[i], errs[i])
		}
	}
	if hits := endpoint.hits.Load(); hits != 1 {
		t.Fatalf("token endpoint hits = %d, want one shared exchange", hits)
	}
}

func TestGoogleAccessTokenFollowsKeyRotation(t *testing.T) {
	key := generateRSAKey(t)
	endpoint := newFakeTokenEndpoint(t, &key.PublicKey)
	secret := vertexSecret(serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL))
	srv, client := newGoogleTestServer(endpoint, secret)

	if _, err := fetchGoogleToken(t, srv, googleTestURI); err != nil {
		t.Fatal(err)
	}
	rotated := "rotated@project.iam.gserviceaccount.com"
	secret.Data["credentials.json"] = serviceAccountKeyJSON(t, key, rotated, endpoint.URL)
	if _, err := client.CoreV1().Secrets("ns1").Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	token, err := fetchGoogleToken(t, srv, googleTestURI)
	if err != nil || token != "ya29.token-2" {
		t.Fatalf("fetch after rotation = %q, %v, want a token for the rotated key", token, err)
	}
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if len(endpoint.claims) != 2 || endpoint.claims[1].Iss != rotated {
		t.Fatalf("assertions = %+v, want the second for %s", endpoint.claims, rotated)
	}
}

func TestGoogleAccessTokenErrors(t *testing.T) {
	key := generateRSAKey(t)
	other := generateRSAKey(t)
	for _, tc := range []struct {
		name      string
		secret    func(endpoint *fakeTokenEndpoint) []byte
		respond   func(w http.ResponseWriter, hit int32)
		expiresIn int
		code      codes.Code
		contains  string
		hits      int32
	}{
		{
			name:     "not a key file",
			secret:   func(*fakeTokenEndpoint) []byte { return []byte("static-token") },
			code:     codes.FailedPrecondition,
			contains: "not a Google service account key",
		},
		{
			name:     "user credential",
			secret:   func(*fakeTokenEndpoint) []byte { return []byte(`{"type":"authorized_user","client_id":"x"}`) },
			code:     codes.FailedPrecondition,
			contains: "authorized_user",
		},
		{
			name: "plaintext token endpoint",
			secret: func(endpoint *fakeTokenEndpoint) []byte {
				return serviceAccountKeyJSON(t, key, googleTestEmail, "http://"+strings.TrimPrefix(endpoint.URL, "https://"))
			},
			code:     codes.FailedPrecondition,
			contains: "https",
		},
		{
			name: "corrupt private key",
			secret: func(endpoint *fakeTokenEndpoint) []byte {
				raw := serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)
				var fields map[string]string
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				fields["private_key"] = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
				raw, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			},
			code:     codes.FailedPrecondition,
			contains: "private_key",
		},
		{
			name: "rejected by Google",
			secret: func(endpoint *fakeTokenEndpoint) []byte {
				return serviceAccountKeyJSON(t, other, googleTestEmail, endpoint.URL)
			},
			code:     codes.FailedPrecondition,
			contains: "invalid_grant: Invalid JWT Signature.",
			hits:     1,
		},
		{
			name: "endpoint failure",
			secret: func(endpoint *fakeTokenEndpoint) []byte {
				return serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)
			},
			respond: func(w http.ResponseWriter, _ int32) {
				http.Error(w, "upstream body must stay out of the status", http.StatusServiceUnavailable)
			},
			code: codes.Unavailable,
			hits: 1,
		},
		{
			name: "token too short lived",
			secret: func(endpoint *fakeTokenEndpoint) []byte {
				return serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)
			},
			expiresIn: 60,
			code:      codes.Unavailable,
			contains:  "outlive the gateway cache",
			hits:      1,
		},
		{
			name: "token without expiry",
			secret: func(endpoint *fakeTokenEndpoint) []byte {
				return serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)
			},
			respond: func(w http.ResponseWriter, _ int32) {
				fmt.Fprint(w, `{"access_token":"ya29.forever","token_type":"Bearer"}`)
			},
			code: codes.Unavailable,
			hits: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := newFakeTokenEndpoint(t, &key.PublicKey)
			endpoint.respond = tc.respond
			if tc.expiresIn != 0 {
				endpoint.expiresIn = tc.expiresIn
			}
			srv, _ := newGoogleTestServer(endpoint, vertexSecret(tc.secret(endpoint)))
			_, err := fetchGoogleToken(t, srv, googleTestURI)
			if status.Code(err) != tc.code {
				t.Fatalf("FetchSecret error = %v, want code %v", err, tc.code)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("FetchSecret error = %v, want it to mention %q", err, tc.contains)
			}
			if strings.Contains(err.Error(), "stay out of the status") {
				t.Fatal("token endpoint response body exposed")
			}
			if hits := endpoint.hits.Load(); hits != tc.hits {
				t.Fatalf("token endpoint hits = %d, want %d", hits, tc.hits)
			}
		})
	}
}

func TestGoogleAccessTokenDeniedBeforeAnyLookup(t *testing.T) {
	key := generateRSAKey(t)
	endpoint := newFakeTokenEndpoint(t, &key.PublicKey)
	srv, client := newGoogleTestServer(endpoint, vertexSecret(serviceAccountKeyJSON(t, key, googleTestEmail, endpoint.URL)))
	_, err := srv.FetchSecret(t.Context(), &credproviderpb.FetchSecretRequest{
		Uri: googleTestURI, ActorSpiffeId: "spiffe://substrate-actor.local/atespace/team-b/actor/vertex",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("FetchSecret error = %v, want PermissionDenied", err)
	}
	if len(client.Actions()) != 0 || endpoint.hits.Load() != 0 {
		t.Fatal("denied request reached Kubernetes or the token endpoint")
	}
}
