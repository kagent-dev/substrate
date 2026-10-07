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
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/atenet"
)

func TestCredentialKeyBinding(t *testing.T) {
	key1 := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	key2 := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	for _, tt := range []struct {
		name                  string
		keys, uids, sequences []string
		wantStatus            int
		wantKey               string
	}{
		{name: "keyless", wantStatus: http.StatusOK, wantKey: key1},
		{name: "keyless with UID", uids: []string{testActorUID}, wantStatus: http.StatusOK, wantKey: key1},
		{name: "same key", sequences: []string{"2"}, keys: []string{key1}, uids: []string{testActorUID}, wantStatus: http.StatusOK, wantKey: key1},
		{name: "replace key", sequences: []string{"3"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusOK, wantKey: key2},
		{name: "maximum sequence", sequences: []string{"18446744073709551615"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusOK, wantKey: key2},

		{name: "older sequence", sequences: []string{"1"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusConflict, wantKey: key1},
		{name: "same sequence different key", sequences: []string{"2"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusConflict, wantKey: key1},
		{name: "same key newer sequence", sequences: []string{"3"}, keys: []string{key1}, uids: []string{testActorUID}, wantStatus: http.StatusConflict, wantKey: key1},
		{name: "same key older sequence", sequences: []string{"1"}, keys: []string{key1}, uids: []string{testActorUID}, wantStatus: http.StatusConflict, wantKey: key1},
		{name: "keyless malformed metadata", sequences: []string{"bad", "0"}, uids: []string{"old-uid", ""}, wantStatus: http.StatusOK, wantKey: key1},
		{name: "missing sequence", sequences: []string{}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "duplicate sequence", sequences: []string{"3", "3"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "empty sequence", sequences: []string{""}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "zero sequence", sequences: []string{"0"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "negative sequence", sequences: []string{"-1"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "signed sequence", sequences: []string{"+3"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "whitespace sequence", sequences: []string{" 3"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "overflow sequence", sequences: []string{"18446744073709551616"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "combined sequence", sequences: []string{"3, 3"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "hexadecimal sequence", sequences: []string{"0x3"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "nondecimal sequence", sequences: []string{"3.0"}, keys: []string{key2}, uids: []string{testActorUID}, wantStatus: http.StatusBadRequest, wantKey: key1},
		{name: "wrong incarnation", sequences: []string{"3"}, keys: []string{key2}, uids: []string{"old-uid"}, wantStatus: http.StatusForbidden, wantKey: key1},
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
			if err := egress.SetCredentialKey(t.Context(), testActorUID, key1, 2); err != nil {
				t.Fatal(err)
			}
			original := egress.active[testActorUID].binding
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
				CredentialBinder:     egress,
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
				if got := testCredentialKey(egress, testActorUID); got != tt.wantKey {
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
			sequences := tt.sequences
			if sequences == nil && len(tt.keys) != 0 {
				sequences = []string{"3"}
			}
			for _, sequence := range sequences {
				req.Header.Add("x-ate-binding-seq", sequence)
			}
			rec := httptest.NewRecorder()
			ingress.ServeConnectHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := testCredentialKey(egress, testActorUID); got != tt.wantKey {
				t.Errorf("key = %q, want %q", got, tt.wantKey)
			}
			if tt.wantKey == key1 && (egress.active[testActorUID].binding != original || original.ctx.Err() != nil) {
				t.Error("unchanged tuple retired or replaced the original binding")
			}
			if got := testCredentialKey(egress, "other-actor"); got != "" {
				t.Errorf("another actor received key %q", got)
			}
			if dialed != (tt.wantStatus == http.StatusOK) {
				t.Fatalf("actor dialed = %t, status = %d", dialed, tt.wantStatus)
			}
			if dialed {
				headers := receiveWithin(t, actorRequests, "actor request")
				for _, name := range []string{CredentialKeyHeader, BindingSequenceHeader, ActorUIDHeader, "Authorization"} {
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
	if err := egress.SetCredentialKey(t.Context(), testActorUID, "key", 1); err == nil {
		t.Fatal("bound key to inactive egress")
	}
	for range 2 {
		if err := egress.Activate(testActorUID, egressDialerFunc(nil), fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got := testCredentialKey(egress, testActorUID); got != "" {
			t.Fatalf("new activation key = %q, want empty", got)
		}
		if err := egress.SetCredentialKey(t.Context(), testActorUID, "key", 1); err != nil {
			t.Fatal(err)
		}
		if err := egress.Deactivate(context.Background(), testActorUID); err != nil {
			t.Fatal(err)
		}
		if got := testCredentialKey(egress, testActorUID); got != "" {
			t.Fatalf("deactivated key = %q, want empty", got)
		}
	}
}

func TestClientCredentialKey(t *testing.T) {
	ca := newTestCA(t)
	cfg := testClientConfig(t, ca)
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", strings.Repeat("A", 43), strings.Repeat("B", 42) + "A"} {
		request := make(chan *http.Request, 1)
		address := serveTestConnectGateway(t, ca, func(conn net.Conn, req *http.Request) {
			request <- req
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		})
		client.dialContext = dialFixedAddress(address)
		conn, err := client.DialContext(t.Context(), "192.0.2.10:443", key)
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
		for _, name := range []string{BindingSequenceHeader, ActorUIDHeader, "Authorization"} {
			if got := req.Header.Get(name); got != "" {
				t.Errorf("CONNECT sent %s: %q", name, got)
			}
		}
	}
}

func testCredentialKey(egress *Egress, uid string) string {
	egress.mu.Lock()
	active := egress.active[uid]
	egress.mu.Unlock()
	if active == nil {
		return ""
	}
	active.bindingMu.Lock()
	defer active.bindingMu.Unlock()
	return active.binding.key
}

// A stale CONNECT must not interrupt a tool call whose mutation has completed
// but whose response is still in flight. Exercise both destination classes;
// this fixture deliberately has no resolver that could mask an atunnel rollback.
func TestDelayedCredentialBindingPreservesActiveCall(t *testing.T) {
	for _, destination := range []string{"192.0.2.10:443", "192.0.2.20:80"} {
		t.Run(destination, func(t *testing.T) {
			key1, key2 := strings.Repeat("A", 43), strings.Repeat("B", 42)+"A"
			ca := newTestCA(t)
			mutated := make(chan struct{})
			release := make(chan struct{})
			releaseResult := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseResult)
			var mutations atomic.Int32
			address := serveTestConnectGateway(t, ca, func(conn net.Conn, connect *http.Request) {
				if got := connect.Header.Get(CredentialKeyHeader); got != key2 {
					t.Errorf("egress key = %q, want K2", got)
				}
				if connect.Host != destination {
					t.Errorf("egress destination = %q, want %q", connect.Host, destination)
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					t.Errorf("reading tool call: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, request.Body)
				_ = request.Body.Close()
				mutations.Add(1)
				close(mutated)
				<-release
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\nresult")
			})
			client := newTestClient(t, ca, WithDialer(dialFixedAddress(address)))
			egress := newBindingEgress(t, destination, client)
			ingress := newBindingIngress(t, egress, nil)
			assertBindingStatus(t, ingress, bindingRequest(key1, "1", testActorUID), http.StatusOK)
			delayed := bindingRequest(key1, "1", testActorUID)
			assertBindingStatus(t, ingress, bindingRequest(key2, "2", testActorUID), http.StatusOK)
			actor := openBindingEgress(t, egress)
			result := make(chan string, 1)
			go func() {
				_, err := io.WriteString(actor, "POST /tool HTTP/1.1\r\nHost: tool\r\nContent-Length: 8\r\n\r\nincrease")
				if err != nil {
					result <- err.Error()
					return
				}
				response, err := http.ReadResponse(bufio.NewReader(actor), nil)
				if err != nil {
					result <- err.Error()
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					result <- err.Error()
					return
				}
				result <- string(body)
			}()
			receiveWithin(t, mutated, "tool mutation")
			assertBindingStatus(t, ingress, delayed, http.StatusConflict)
			assertBindingStatus(t, ingress, bindingRequest(key2, "2", testActorUID), http.StatusOK)
			assertBindingStatus(t, ingress, bindingRequest("", "", ""), http.StatusOK)
			if got := testCredentialKey(egress, testActorUID); got != key2 {
				t.Fatalf("delayed CONNECT replaced K2: %q", got)
			}
			releaseResult()
			if got := receiveWithin(t, result, "tool response"); got != "result" {
				t.Fatalf("tool response = %q, want result", got)
			}
			if got := mutations.Load(); got != 1 {
				t.Fatalf("mutation count = %d, want 1", got)
			}
		})
	}
}

func TestCredentialReplacementClosesBeforeForwarding(t *testing.T) {
	peers := make(chan net.Conn, 4)
	egress := newBindingEgress(t, "192.0.2.10:443", egressDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		upstream, peer := net.Pipe()
		peers <- peer
		return upstream, nil
	}))
	var old []net.Conn
	ingress := newBindingIngress(t, egress, func() {
		for _, conn := range old {
			assertConnectionClosed(t, conn)
		}
	})
	// The first binding also closes keyless startup connections. Each subsequent
	// input opens fresh connections after the previous pool has been retired.
	for sequence, key := range []string{strings.Repeat("A", 43), strings.Repeat("B", 42) + "A"} {
		old = nil
		for range 2 {
			actor := openBindingEgress(t, egress)
			peer := receiveWithin(t, peers, "gateway connection")
			t.Cleanup(func() { _ = peer.Close() })
			assertConnectionCarries(t, actor, peer, "ready")
			old = append(old, actor, peer)
		}
		assertBindingStatus(t, ingress, bindingRequest(key, strconv.Itoa(sequence+1), testActorUID), http.StatusOK)
	}
}

func TestCredentialBindingFencesLateDial(t *testing.T) {
	for _, deactivate := range []bool{false, true} {
		t.Run(fmt.Sprintf("deactivate=%t", deactivate), func(t *testing.T) {
			started, release := make(chan string, 1), make(chan struct{})
			upstream, peer := net.Pipe()
			defer peer.Close()
			dialer := egressDialerFunc(func(_ context.Context, _, key string) (net.Conn, error) {
				started <- key
				<-release // Deliberately ignore cancellation until the dial finishes.
				return upstream, nil
			})
			egress := newBindingEgress(t, "192.0.2.10:443", dialer)
			releaseDial := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseDial)
			if err := egress.SetCredentialKey(t.Context(), testActorUID, "K1", 1); err != nil {
				t.Fatal(err)
			}
			active := egress.active[testActorUID]
			actor := openBindingEgress(t, egress)
			if got := receiveWithin(t, started, "old dial"); got != "K1" {
				t.Fatalf("dial captured %q, want K1", got)
			}
			deactivated := make(chan error, 1)
			if deactivate {
				go func() { deactivated <- egress.Deactivate(t.Context(), testActorUID) }()
				receiveWithin(t, active.ctx.Done(), "activation cancellation")
				if err := egress.Activate(testActorUID, dialer, fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if err := egress.SetCredentialKey(t.Context(), testActorUID, "K2", 2); err != nil {
				t.Fatal(err)
			}
			assertConnectionClosed(t, actor)
			releaseDial()
			assertConnectionClosed(t, peer)
			if deactivate {
				if err := receiveWithin(t, deactivated, "deactivation"); err != nil {
					t.Fatal(err)
				}
			}
			if got := testCredentialKey(egress, testActorUID); got != "K2" {
				t.Fatalf("late dial changed new binding: %q", got)
			}
		})
	}
}

func TestClientCancelsStalledConnect(t *testing.T) {
	ca := newTestCA(t)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	address := serveTestConnectGateway(t, ca, func(net.Conn, *http.Request) {
		close(started)
		<-release
	})
	client := newTestClient(t, ca, WithDialer(dialFixedAddress(address)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		conn, err := client.DialContext(ctx, "192.0.2.10:443", strings.Repeat("A", 43))
		if conn != nil {
			_ = conn.Close()
		}
		finished <- err
	}()
	receiveWithin(t, started, "gateway CONNECT")
	cancel()
	if err := receiveWithin(t, finished, "canceled CONNECT"); err == nil {
		t.Fatal("canceled CONNECT succeeded")
	}
}

func newBindingEgress(t *testing.T, destination string, dialer egressDialer) *Egress {
	t.Helper()
	egress, err := NewEgress(func(net.Conn) (string, error) { return destination, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := egress.Activate(testActorUID, dialer, fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := egress.Deactivate(ctx, testActorUID); err != nil {
			t.Error(err)
		}
	})
	return egress
}

func newBindingIngress(t *testing.T, egress *Egress, onDial func()) *Server {
	t.Helper()
	ingress := newTestServer(t, &url.URL{Scheme: "http", Host: "127.0.0.1:8080"})
	ingress.credentialBinder = egress
	if err := ingress.Activate("team-a", "actor-1", testActorUID, func(context.Context, string, string) (net.Conn, error) {
		if onDial != nil {
			onDial()
		}
		conn, peer := net.Pipe()
		_ = peer.Close()
		return conn, nil
	}); err != nil {
		t.Fatal(err)
	}
	return ingress
}

func bindingRequest(key, sequence, uid string) *http.Request {
	req := httptest.NewRequest(http.MethodConnect, "https://worker/", nil)
	req.ProtoMajor, req.ProtoMinor, req.Proto = 2, 0, "HTTP/2.0"
	req.Host = "actor:8080"
	req.Header.Set(atenet.TargetActorHeader, "team-a/actor-1")
	if key != "" {
		req.Header.Set("x-ate-credential-key", key)
		req.Header.Set("x-ate-binding-seq", sequence)
		req.Header.Set("x-ate-actor-uid", uid)
	}
	return req
}

func assertBindingStatus(t *testing.T, ingress *Server, req *http.Request, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ingress.ServeConnectHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("CONNECT status = %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
}

func openBindingEgress(t *testing.T, egress *Egress) net.Conn {
	t.Helper()
	actor, proxy := net.Pipe()
	_ = actor.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { _ = actor.Close() })
	egress.mu.Lock()
	active := egress.active[testActorUID]
	egress.mu.Unlock()
	egress.handle(proxy, active)
	return actor
}

func assertConnectionCarries(t *testing.T, source, destination net.Conn, message string) {
	t.Helper()
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(source, message); written <- err }()
	_ = destination.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len(message))
	if _, err := io.ReadFull(destination, got); err != nil {
		t.Fatal(err)
	}
	if err := receiveWithin(t, written, "egress write"); err != nil {
		t.Fatal(err)
	}
	if string(got) != message {
		t.Fatalf("egress body = %q, want %q", got, message)
	}
}

func assertConnectionClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("connection was not closed: %v", err)
	}
}

func TestCredentialRetryWaitsForClosure(t *testing.T) {
	key1, key2 := strings.Repeat("A", 43), strings.Repeat("B", 42)+"A"
	upstream, peer := net.Pipe()
	defer peer.Close()
	egress := newBindingEgress(t, "192.0.2.10:443", egressDialerFunc(func(context.Context, string, string) (net.Conn, error) { return upstream, nil }))
	ingress := newBindingIngress(t, egress, nil)
	assertBindingStatus(t, ingress, bindingRequest(key1, "1", testActorUID), http.StatusOK)
	actor, downstream := net.Pipe()
	defer actor.Close()
	closing, release := make(chan struct{}), make(chan struct{})
	releaseClose := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseClose)
	egress.handle(&blockingCloseConn{Conn: downstream, closing: closing, release: release}, egress.active[testActorUID])
	assertConnectionCarries(t, actor, peer, "ready")
	first := make(chan error, 1)
	go func() { first <- egress.SetCredentialKey(t.Context(), testActorUID, key2, 2) }()
	receiveWithin(t, closing, "old connection closure")
	retryEntered, retryDone := make(chan context.Context, 1), make(chan int, 1)
	ingress.credentialBinder = &delayedCredentialBinder{CredentialBinder: egress, entered: retryEntered}
	go func() {
		rec := httptest.NewRecorder()
		ingress.ServeConnectHTTP(rec, bindingRequest(key2, "2", testActorUID))
		retryDone <- rec.Code
	}()
	receiveWithin(t, retryEntered, "retry binding check")
	select {
	case code := <-retryDone:
		t.Fatalf("retry forwarded before closure finished: status %d", code)
	case <-time.After(50 * time.Millisecond):
	}
	releaseClose()
	if err := receiveWithin(t, first, "replacement"); err != nil {
		t.Fatal(err)
	}
	if code := receiveWithin(t, retryDone, "retry after closure"); code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
}

type blockingCloseConn struct {
	net.Conn
	closing chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *blockingCloseConn) Close() error {
	c.once.Do(func() { close(c.closing) })
	<-c.release
	return c.Conn.Close()
}

func TestOldIngressCannotBindReactivatedActor(t *testing.T) {
	key1, key2 := strings.Repeat("A", 43), strings.Repeat("B", 42)+"A"
	egress := newBindingEgress(t, "192.0.2.10:443", egressDialerFunc(nil))
	ingress := newBindingIngress(t, egress, nil)
	entered, release := make(chan context.Context, 1), make(chan struct{})
	releaseBind := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseBind)
	ingress.credentialBinder = &delayedCredentialBinder{CredentialBinder: egress, entered: entered, release: release}
	oldRequestDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		// Even a higher sequence cannot cross an activation boundary.
		ingress.ServeConnectHTTP(rec, bindingRequest(key1, "3", testActorUID))
		oldRequestDone <- rec.Code
	}()
	oldContext := receiveWithin(t, entered, "old ingress binding")
	deactivated := make(chan error, 1)
	go func() { deactivated <- ingress.Deactivate(t.Context(), "team-a", "actor-1", testActorUID) }()
	receiveWithin(t, oldContext.Done(), "old ingress cancellation")
	if err := egress.Deactivate(t.Context(), testActorUID); err != nil {
		t.Fatal(err)
	}
	if err := egress.Activate(testActorUID, egressDialerFunc(nil), fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := egress.SetCredentialKey(t.Context(), testActorUID, key2, 2); err != nil {
		t.Fatal(err)
	}
	releaseBind()
	if code := receiveWithin(t, oldRequestDone, "old ingress request"); code != http.StatusServiceUnavailable {
		t.Fatalf("old ingress status = %d, want 503", code)
	}
	if err := receiveWithin(t, deactivated, "old ingress deactivation"); err != nil {
		t.Fatal(err)
	}
	if got := testCredentialKey(egress, testActorUID); got != key2 {
		t.Fatalf("old ingress changed new activation key: %q", got)
	}
}

func TestCredentialBindingAfterRestart(t *testing.T) {
	key1, key2 := strings.Repeat("A", 43), strings.Repeat("B", 42)+"A"
	egress := newBindingEgress(t, "192.0.2.10:443", egressDialerFunc(nil))
	ingress := newBindingIngress(t, egress, nil)
	assertBindingStatus(t, ingress, bindingRequest(key2, "2", testActorUID), http.StatusOK)
	if err := ingress.Deactivate(t.Context(), "team-a", "actor-1", testActorUID); err != nil {
		t.Fatal(err)
	}
	if err := egress.Deactivate(t.Context(), testActorUID); err != nil {
		t.Fatal(err)
	}
	if err := egress.Activate(testActorUID, egressDialerFunc(nil), fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ingress = newBindingIngress(t, egress, nil)
	// With no local ordering history, only the resolver can establish currentness
	// for a stale first tuple with the same UID. A different UID still fails here.
	assertBindingStatus(t, ingress, bindingRequest(key1, "1", "old-uid"), http.StatusForbidden)
	if got := testCredentialKey(egress, testActorUID); got != "" {
		t.Fatalf("wrong UID installed key %q", got)
	}
	assertBindingStatus(t, ingress, bindingRequest(key1, "1", testActorUID), http.StatusOK)
	assertBindingStatus(t, ingress, bindingRequest(key2, "2", testActorUID), http.StatusOK)
}

type delayedCredentialBinder struct {
	CredentialBinder
	entered chan<- context.Context
	release <-chan struct{}
}

func (b *delayedCredentialBinder) SetCredentialKey(ctx context.Context, uid, key string, sequence uint64) error {
	b.entered <- ctx
	if b.release != nil {
		<-b.release
	}
	return b.CredentialBinder.SetCredentialKey(ctx, uid, key, sequence)
}
