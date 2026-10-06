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

package atunnel

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/atenet"
)

func TestCredentialKeyBinding(t *testing.T) {
	key1 := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	key2 := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	for _, tt := range []struct {
		name       string
		keys, uids []string
		wantStatus int
		wantKey    string
	}{
		{name: "keyless", wantStatus: http.StatusOK, wantKey: key1},
		{name: "keyless with UID", uids: []string{testActorUID}, wantStatus: http.StatusOK, wantKey: key1},
		{name: "same key", keys: []string{key1}, uids: []string{testActorUID}, wantStatus: http.StatusOK, wantKey: key1},
		{name: "replace key", keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusOK, wantKey: key2},
		{name: "wrong incarnation", keys: []string{key2}, uids: []string{"old-uid"}, wantStatus: http.StatusForbidden, wantKey: key1},
		{name: "missing UID", keys: []string{key2}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "empty UID", keys: []string{key2}, uids: []string{""}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "duplicate UID", keys: []string{key2}, uids: []string{testActorUID, testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "duplicate key", keys: []string{key2, key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "empty key", keys: []string{""}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "short key", keys: []string{"abc"}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "invalid base64url", keys: []string{strings.Repeat("+", 43)}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "padded key", keys: []string{key2 + "="}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "noncanonical key", keys: []string{strings.Repeat("A", 42) + "B"}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "combined keys", keys: []string{key1 + ", " + key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			egress, err := NewEgress(func(net.Conn) (string, error) { return "192.0.2.10:443", nil })
			if err != nil {
				t.Fatal(err)
			}
			for _, uid := range []string{testActorUID, "other-actor"} {
				if err := egress.Activate(uid, egressDialerFunc(nil), fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = egress.Deactivate(context.Background(), uid) })
			}
			if err := egress.SetCredentialKey(testActorUID, key1); err != nil {
				t.Fatal(err)
			}
			actorRequests := make(chan http.Header, 1)
			actor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				actorRequests <- r.Header.Clone()
				_, _ = io.WriteString(w, "actor response")
			}))
			defer actor.Close()
			upstream, err := url.Parse(actor.URL)
			if err != nil {
				t.Fatal(err)
			}
			bundle, trust := makeCertFiles(t, t.TempDir())
			ingress, err := NewServer(Config{
				SetCredentialKey:     egress.SetCredentialKey,
				CredentialBundlePath: bundle,
				TrustBundlePath:      trust,
				AllowedClientID:      "spiffe://cluster.local/ns/ate-system/sa/atenet-router",
				Upstream:             upstream,
			})
			if err != nil {
				t.Fatal(err)
			}
			dialed := false
			if err := ingress.Activate("team-a", "actor-1", testActorUID, func(ctx context.Context, network, address string) (net.Conn, error) {
				dialed = true
				if got := egress.CredentialKey(testActorUID); got != tt.wantKey {
					t.Errorf("key at actor dial = %q, want %q", got, tt.wantKey)
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodConnect, "https://worker/", strings.NewReader("GET / HTTP/1.1\r\nHost: actor\r\nConnection: close\r\n\r\n"))
			req.ProtoMajor, req.ProtoMinor, req.Proto = 2, 0, "HTTP/2.0"
			req.Host = upstream.Host
			req.Header.Set(atenet.TargetActorHeader, "team-a/actor-1")
			for _, key := range tt.keys {
				req.Header.Add("x-ate-credential-key", key)
			}
			for _, uid := range tt.uids {
				req.Header.Add("x-ate-actor-uid", uid)
			}
			rec := httptest.NewRecorder()
			ingress.ServeConnectHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := egress.CredentialKey(testActorUID); got != tt.wantKey {
				t.Errorf("key = %q, want %q", got, tt.wantKey)
			}
			if got := egress.CredentialKey("other-actor"); got != "" {
				t.Errorf("another actor received key %q", got)
			}
			if dialed != (tt.wantStatus == http.StatusOK) {
				t.Fatalf("actor dialed = %t, status = %d", dialed, tt.wantStatus)
			}
			if dialed {
				headers := receiveWithin(t, actorRequests, "actor request")
				for _, name := range []string{CredentialKeyHeader, ActorUIDHeader, "Authorization"} {
					if got := headers.Get(name); got != "" {
						t.Errorf("actor received %s: %q", name, got)
					}
				}
				if !strings.Contains(rec.Body.String(), "actor response") {
					t.Errorf("missing actor response: %s", rec.Body.String())
				}
			}
		})
	}
}

func TestEgressCredentialKeyLifecycle(t *testing.T) {
	egress, err := NewEgress(func(net.Conn) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := egress.SetCredentialKey(testActorUID, "key"); err == nil {
		t.Fatal("bound key to inactive egress")
	}
	for range 2 {
		if err := egress.Activate(testActorUID, egressDialerFunc(nil), fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got := egress.CredentialKey(testActorUID); got != "" {
			t.Fatalf("new activation key = %q, want empty", got)
		}
		if err := egress.SetCredentialKey(testActorUID, "key"); err != nil {
			t.Fatal(err)
		}
		if err := egress.Deactivate(context.Background(), testActorUID); err != nil {
			t.Fatal(err)
		}
		if got := egress.CredentialKey(testActorUID); got != "" {
			t.Fatalf("deactivated key = %q, want empty", got)
		}
	}
}

func TestClientCredentialKey(t *testing.T) {
	ca := newTestCA(t)
	egress, err := NewEgress(func(net.Conn) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := egress.Activate(testActorUID, egressDialerFunc(nil), fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = egress.Deactivate(context.Background(), testActorUID) })
	cfg := testClientConfig(t, ca)
	cfg.CredentialKey = func() string { return egress.CredentialKey(testActorUID) }
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", strings.Repeat("A", 43), strings.Repeat("B", 42) + "A"} {
		if key != "" {
			if err := egress.SetCredentialKey(testActorUID, key); err != nil {
				t.Fatal(err)
			}
		}
		request := make(chan *http.Request, 1)
		address := serveTestConnectGateway(t, ca, func(conn net.Conn, req *http.Request) {
			request <- req
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		})
		client.dialContext = dialFixedAddress(address)
		conn, err := client.DialContext(t.Context(), "192.0.2.10:443")
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		req := receiveWithin(t, request, "egress CONNECT")
		if got := req.Header.Get("x-ate-credential-key"); got != key {
			t.Errorf("CONNECT key = %q, want %q", got, key)
		}
		if key == "" && len(req.Header.Values(CredentialKeyHeader)) != 0 {
			t.Error("keyless CONNECT sent a credential key header")
		}
		for _, name := range []string{ActorUIDHeader, "Authorization"} {
			if got := req.Header.Get(name); got != "" {
				t.Errorf("CONNECT sent %s: %q", name, got)
			}
		}
	}
}
