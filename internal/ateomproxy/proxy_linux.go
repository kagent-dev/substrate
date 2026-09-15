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

// Package ateomproxy owns the worker's sandbox networking and atunnel lifecycle.
package ateomproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/atunnel"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/resources"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	WorkerUID string
	MicroVM   bool
	// Ingress and CONNECT listen in the worker namespace. Capture listens in
	// Gateway and receives redirected sandbox TCP; DNS uses Gateway port 53.
	IngressAddress, ConnectAddress, CaptureAddress string
	Ingress                                        atunnel.Config
	EgressTrustBundlePath                          string
}

// Proxy owns the worker listeners, namespace sockets, and network topology.
// It lives for the worker's lifetime; a Session lives with its assigned workload.
// Prepare enables egress (including DNS) for startup, PublishIngress admits
// inbound work after readiness, and Deactivate stops traffic before checkpoint.
// Runtime RPCs serialize Prepare, Deactivate, Reset, and Session operations.
type Proxy struct {
	Net                   *ateomnet.Sandbox
	Ingress               *atunnel.Server
	HTTPClient            *http.Client
	egress                *atunnel.Egress
	port                  uint16
	broker                atunnel.BrokerConfig
	egressTrustBundlePath string
	listeners             []io.Closer
	cancel                context.CancelFunc
	group                 errgroup.Group
	closeOnce             sync.Once
	closeErr              error
}

func New(ctx context.Context, cfg Config) (_ *Proxy, retErr error) {
	listen, err := netip.ParseAddrPort(cfg.CaptureAddress)
	if err != nil || !listen.Addr().Is4() || listen.Port() == 53 {
		return nil, fmt.Errorf("invalid capture address %q", cfg.CaptureAddress)
	}
	n, err := ateomnet.NewSandbox(ctx, cfg.WorkerUID, cfg.MicroVM)
	if err != nil {
		return nil, err
	}
	// Readiness uses the same namespace dialer as ingress: the actor's address
	// is reachable from Gateway, not from the worker's own network namespace.
	p := &Proxy{
		Net:                   n,
		HTTPClient:            &http.Client{Transport: &http.Transport{DialContext: n.DialContext, DisableCompression: true, MaxIdleConnsPerHost: 1}},
		broker:                atunnel.BrokerConfig{SocketPath: ateompath.CredentialBrokerSocket, CredentialBundlePath: cfg.Ingress.CredentialBundlePath, TrustBundlePath: cfg.Ingress.TrustBundlePath},
		egressTrustBundlePath: cfg.EgressTrustBundlePath,
	}
	ctx, p.cancel = context.WithCancel(ctx)
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, p.Close())
		}
	}()
	cfg.Ingress.DialContext = n.DialContext
	p.Ingress, err = atunnel.NewServer(cfg.Ingress)
	if err != nil {
		return nil, err
	}
	p.egress, err = atunnel.NewEgress(atunnel.TCPOriginalDestination)
	if err != nil {
		return nil, err
	}
	// Acquire every listener before starting any serving loop, registering each
	// for rollback immediately. A port conflict must not leave a partial proxy.
	ingress, err := net.Listen("tcp", cfg.IngressAddress)
	if err != nil {
		return nil, fmt.Errorf("opening ingress listener: %w", err)
	}
	p.listeners = append(p.listeners, ingress)
	connect, err := net.Listen("tcp", cfg.ConnectAddress)
	if err != nil {
		return nil, fmt.Errorf("opening CONNECT listener: %w", err)
	}
	p.listeners = append(p.listeners, connect)
	capture, err := ateomnet.ListenTCP(n.Gateway, listen)
	if err != nil {
		return nil, err
	}
	p.listeners = append(p.listeners, capture)
	p.port = uint16(capture.Addr().(*net.TCPAddr).Port)
	// Read the worker's resolver before serving DNS. Queries reach it through
	// authenticated actor egress, not through an unrestricted worker socket.
	resolver, err := atunnel.DNSResolver()
	if err != nil {
		return nil, err
	}
	udp, err := ateomnet.ListenUDP(n.Gateway, netip.MustParseAddrPort("0.0.0.0:53"))
	if err != nil {
		return nil, err
	}
	p.listeners = append(p.listeners, udp)
	tcp, err := ateomnet.ListenTCP(n.Gateway, netip.MustParseAddrPort("0.0.0.0:53"))
	if err != nil {
		return nil, err
	}
	p.listeners = append(p.listeners, tcp)
	p.group.Go(func() error {
		<-ctx.Done()
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return p.Deactivate(cleanup)
	})
	// Any serving loop ending stops the others. Wait surfaces failure to the worker.
	p.group.Go(func() error { defer p.cancel(); return p.Ingress.Serve(ctx, ingress) })
	p.group.Go(func() error { defer p.cancel(); return p.Ingress.ServeConnect(ctx, connect) })
	p.group.Go(func() error { defer p.cancel(); return p.egress.Serve(ctx, capture) })
	p.group.Go(func() error { defer p.cancel(); return atunnel.ServeDNS(ctx, udp, tcp, resolver, p.egress.DialContext) })
	return p, nil
}

// Wait returns when proxy serving stops. The worker must stop accepting work then.
func (p *Proxy) Wait() error { return p.group.Wait() }

// Close stops serving and drains handlers before releasing namespace handles.
// Reset and Deactivate keep listeners alive for reuse; Close ends the worker's
// proxy lifetime and is also safe for partially constructed proxies.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p.closeErr = p.Deactivate(ctx)
		for _, listener := range p.listeners {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				p.closeErr = errors.Join(p.closeErr, err)
			}
		}
		_ = p.group.Wait() // Wait reports serving errors to the worker.
		p.closeErr = errors.Join(p.closeErr, p.Net.Close())
	})
	return p.closeErr
}

// Deactivate drains traffic and closes the readiness connection pool, keeping
// the topology and session credentials available for checkpoint rollback.
func (p *Proxy) Deactivate(ctx context.Context) error {
	p.HTTPClient.CloseIdleConnections()
	var errs []error
	if p.Ingress != nil {
		errs = append(errs, p.Ingress.Deactivate(ctx))
	}
	if p.egress != nil {
		errs = append(errs, p.egress.Deactivate(ctx))
	}
	return errors.Join(errs...)
}

// Reset removes the sandbox-facing device after the runtime has stopped.
func (p *Proxy) Reset(ctx context.Context) error {
	return errors.Join(p.Deactivate(ctx), p.Net.Reset(ctx))
}

// Session belongs to the runtime's live workload record. Credentials are kept
// here so a failed checkpoint can resume networking with its renewed certificate.
type Session struct {
	proxy             *Proxy
	actor             resources.ActorRef
	client            *atunnel.Client
	certificateSource *atunnel.BrokerCertificateSource
}

// Prepare installs capture and authenticates egress before workload startup.
// On failure it rolls back all networking; ingress is published after readiness.
// Startup may itself require DNS or egress, so waiting for readiness before
// activation would deadlock those workloads. Without a gateway, capture stays
// installed but egress rejects connections; there is no direct-dial fallback.
func (p *Proxy) Prepare(ctx context.Context, actor resources.ActorAttribution, gateway *ateompb.EgressGateway) (_ *Session, retErr error) {
	defer func() {
		if retErr != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			retErr = errors.Join(retErr, p.Reset(cleanup))
		}
	}()
	if err := p.Net.Setup(ctx, p.port); err != nil {
		return nil, fmt.Errorf("setting up actor network: %w", err)
	}
	s := &Session{proxy: p, actor: actor.Ref}
	if gateway == nil {
		return s, nil
	}
	serverName, _, err := net.SplitHostPort(gateway.GetAddress())
	if err != nil {
		return nil, fmt.Errorf("invalid egress gateway address %q: %w", gateway.GetAddress(), err)
	}
	broker := p.broker
	broker.ActorAtespace, broker.ActorName, broker.ActorUID = actor.Ref.Atespace, actor.Ref.Name, actor.UID
	s.certificateSource, err = atunnel.NewBrokerCertificateSource(broker)
	if err != nil {
		return nil, fmt.Errorf("configuring actor certificate broker: %w", err)
	}
	expiresAt, err := s.certificateSource.MintAteomCertificate(ctx)
	if err != nil {
		return nil, fmt.Errorf("obtaining actor certificate: %w", err)
	}
	s.client, err = atunnel.NewClient(atunnel.ClientConfig{GatewayAddress: gateway.GetAddress(), ServerName: serverName, GetClientCertificate: s.certificateSource.GetClientCertificate, TrustBundlePath: p.egressTrustBundlePath}, atunnel.WithDialer(p.Net.DialEndpoint))
	if err != nil {
		return nil, fmt.Errorf("configuring actor egress client: %w", err)
	}
	if err := p.egress.Activate(s.client, s.certificateSource, expiresAt); err != nil {
		return nil, err
	}
	return s, nil
}

// PublishIngress admits incoming requests only after the runtime has confirmed
// application readiness. It does not affect the already-active startup egress.
func (s *Session) PublishIngress() error {
	return s.proxy.Ingress.Activate(s.actor.Atespace, s.actor.Name)
}

// Resume rolls back network deactivation after a failed checkpoint. The runtime
// must still be alive. A canceled checkpoint RPC must not prevent recovery, so
// use a separate bounded deadline while retaining the caller's context values.
func (s *Session) Resume(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if s.client != nil {
		cert, err := s.certificateSource.GetClientCertificate(nil)
		var expiresAt time.Time
		if err == nil {
			expiresAt = cert.Leaf.NotAfter
		} else {
			// Renewal was paused during checkpointing; the cached cert may have expired.
			expiresAt, err = s.certificateSource.MintAteomCertificate(ctx)
			if err != nil {
				return err
			}
		}
		if err := s.proxy.egress.Activate(s.client, s.certificateSource, expiresAt); err != nil {
			return err
		}
	}
	return s.PublishIngress()
}
