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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// egressDialer opens an authenticated tunnel to an original destination.
type egressDialer interface {
	DialContext(ctx context.Context, destination, credentialKey string) (net.Conn, error)
}

type actorCertificateSource interface {
	MintAteomCertificate(context.Context) (time.Time, error)
}

// OriginalDestination returns the address that a transparently intercepted
// connection originally targeted.
type OriginalDestination func(net.Conn) (string, error)

// Egress proxies actor TCP connections through an egress CONNECT dialer. It is
// long-lived across actor activations, but only carries traffic while an actor
// is assigned to its worker.
type Egress struct {
	originalDestination OriginalDestination

	mu sync.Mutex
	// Keyed by actor UID, supplied by the namespace-specific listener.
	active map[string]*egressActivation
}

type egressActivation struct {
	// Serialize replacement and connection registration within this activation.
	// Hold through closure so a retry cannot forward before old connections close.
	bindingMu sync.Mutex
	binding   *credentialBinding

	dialer            egressDialer
	certificateSource actorCertificateSource
	expiresAt         time.Time

	// ctx scopes certificate renewal and every tunnel opened by this activation. wg
	// lets Deactivate wait until both renewal and tunnel forwarding have exited.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewEgress creates an activation-aware egress proxy.
func NewEgress(originalDestination OriginalDestination) (*Egress, error) {
	if originalDestination == nil {
		return nil, fmt.Errorf("atunnel: original destination resolver is required")
	}
	return &Egress{
		originalDestination: originalDestination,
		active:              map[string]*egressActivation{},
	}, nil
}

// SetCredentialKey installs a newer binding and closes the previous binding's
// connections before returning. ctx is the ingress activation's lifetime.
func (e *Egress) SetCredentialKey(ctx context.Context, actorUID, key string, sequence uint64) error {
	e.mu.Lock()
	active := e.active[actorUID]
	if active == nil || active.dialer == nil || ctx.Err() != nil {
		e.mu.Unlock()
		return fmt.Errorf("atunnel: actor egress is not active")
	}
	e.mu.Unlock()

	active.bindingMu.Lock()
	defer active.bindingMu.Unlock()
	if err := errors.Join(ctx.Err(), active.ctx.Err()); err != nil {
		return err
	}
	old := active.binding
	if key == "" || sequence == 0 {
		return errBindingConflict
	}
	if old.key == key && old.sequence == sequence {
		return nil
	}
	if old.key != "" && (sequence <= old.sequence || key == old.key) {
		return errBindingConflict
	}
	old.close()
	active.binding = newCredentialBinding(active.ctx, key, sequence)
	return nil
}

// Bind captures the actor's activation before its listeners start serving.
func (e *Egress) Bind(actorUID string) (func(context.Context, net.Listener) error, error) {
	if actorUID == "" {
		return nil, fmt.Errorf("atunnel: actor UID is required")
	}
	e.mu.Lock()
	active := e.activationLocked(actorUID)
	e.mu.Unlock()
	return func(ctx context.Context, listener net.Listener) error {
		defer func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.active[actorUID] == active && active.dialer == nil {
				delete(e.active, actorUID)
				active.cancel()
			}
		}()
		return e.serve(ctx, listener, active)
	}, nil
}

func (e *Egress) activationLocked(actorUID string) *egressActivation {
	active := e.active[actorUID]
	if active == nil {
		ctx, cancel := context.WithCancel(context.Background())
		active = &egressActivation{ctx: ctx, cancel: cancel}
		active.binding = newCredentialBinding(ctx, "", 0)
		e.active[actorUID] = active
	}
	return active
}

func (e *Egress) serve(ctx context.Context, listener net.Listener, active *egressActivation) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-active.ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	defer close(done)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("atunnel: accepting actor egress connection: %w", err)
		}
		e.handle(conn, active)
	}
}

// Activate enables the actor's egress and certificate renewal until deactivation.
func (e *Egress) Activate(actorUID string, dialer egressDialer, certificateSource actorCertificateSource, expiresAt time.Time) error {
	if actorUID == "" {
		return fmt.Errorf("atunnel: actor UID is required")
	}
	if dialer == nil {
		return fmt.Errorf("atunnel: egress dialer is required")
	}
	if certificateSource == nil {
		return fmt.Errorf("atunnel: actor certificate source is required")
	}
	if !expiresAt.After(time.Now()) {
		return fmt.Errorf("atunnel: valid actor certificate is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	active := e.activationLocked(actorUID)
	if active.dialer != nil {
		return fmt.Errorf("atunnel: actor %s already has active egress", actorUID)
	}
	active.dialer = dialer
	active.certificateSource = certificateSource
	active.expiresAt = expiresAt
	active.wg.Add(1)
	go e.renew(active, expiresAt)
	return nil
}

func (e *Egress) renew(active *egressActivation, expiresAt time.Time) {
	defer active.wg.Done()
	// Schedule from the credential's remaining lifetime: renew at 90%, then
	// keep retrying after expiry so egress can recover without reactivation.
	delay := renewAfter(expiresAt)
	expired := false
	for waitForRenewal(active.ctx, delay) {
		if !expiresAt.After(time.Now()) && !expired {
			slog.WarnContext(active.ctx, "Atunnel actor certificate expired; blocking new egress connections",
				slog.Time("expiredAt", expiresAt))
			expired = true
		}
		nextExpiry, err := active.certificateSource.MintAteomCertificate(active.ctx)
		if err != nil {
			code := status.Code(err)
			// Don't retry on these codes.  (No retries in ateom will fix
			// Aborted, it indicates that ateom is running an out-of-date
			// actor.)
			if code == codes.Aborted || code == codes.FailedPrecondition || code == codes.PermissionDenied {
				e.mu.Lock()
				active.expiresAt = time.Time{}
				e.mu.Unlock()
				slog.WarnContext(active.ctx, "Atunnel actor certificate renewal was denied; blocking new egress connections",
					slog.Any("err", err))
				return
			}
			delay = retryAfter(expiresAt)
			continue
		}
		if !nextExpiry.After(time.Now()) {
			delay = retryAfter(expiresAt)
			continue
		}
		e.mu.Lock()
		// Check cancellation under the same lock as Deactivate. Whichever wins
		// the lock last either installs a live expiry or leaves the activation empty;
		// renewal can never restore a credential after deactivation cleared it.
		if active.ctx.Err() != nil {
			e.mu.Unlock()
			return
		}
		active.expiresAt = nextExpiry
		e.mu.Unlock()
		if expired {
			slog.InfoContext(active.ctx, "Atunnel actor certificate renewed; allowing new egress connections",
				slog.Time("expiresAt", nextExpiry))
			expired = false
		}
		expiresAt = nextExpiry
		delay = renewAfter(expiresAt)
	}
}

func renewAfter(expiresAt time.Time) time.Duration {
	remaining := time.Until(expiresAt)
	return remaining - remaining/10
}

func retryAfter(expiresAt time.Time) time.Duration {
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return 25*time.Second + rand.N(10*time.Second)
	}
	return min(30*time.Second, max(time.Second, remaining/10), remaining)
}

func waitForRenewal(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Deactivate disables the actor's egress and closes and drains its streams.
func (e *Egress) Deactivate(ctx context.Context, actorUID string) error {
	e.mu.Lock()
	active := e.active[actorUID]
	delete(e.active, actorUID)
	if active != nil {
		active.expiresAt = time.Time{}
		active.cancel()
	}
	e.mu.Unlock()
	if active == nil {
		return nil
	}
	active.bindingMu.Lock()
	active.binding.close()
	active.bindingMu.Unlock()

	done := make(chan struct{})
	go func() {
		active.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("atunnel: waiting for active egress streams to stop: %w", ctx.Err())
	}
}

func (e *Egress) handle(downstream net.Conn, active *egressActivation) {
	e.mu.Lock()
	if active == nil || active.ctx.Err() != nil {
		e.mu.Unlock()
		_ = downstream.Close()
		return
	}
	if time.Now().Compare(active.expiresAt) >= 0 {
		// Expiry blocks only new tunnels. Connections admitted with a valid
		// certificate have completed mTLS and are allowed to drain normally.
		e.mu.Unlock()
		_ = downstream.Close()
		return
	}
	active.wg.Add(1)
	e.mu.Unlock()

	active.bindingMu.Lock()
	if active.ctx.Err() != nil {
		active.bindingMu.Unlock()
		active.wg.Done()
		_ = downstream.Close()
		return
	}
	binding := active.binding
	conn := &egressConnection{downstream: downstream}
	binding.conns[conn] = struct{}{}
	active.bindingMu.Unlock()

	go func() {
		defer active.wg.Done()
		defer func() {
			active.bindingMu.Lock()
			defer active.bindingMu.Unlock()
			delete(binding.conns, conn)
			conn.close()
		}()

		destination, err := e.originalDestination(downstream)
		if err != nil {
			slog.WarnContext(active.ctx, "atunnel failed to resolve original egress destination", slog.Any("err", err))
			return
		}
		upstream, err := active.dialer.DialContext(binding.ctx, destination, binding.key)
		if err != nil {
			slog.WarnContext(active.ctx, "atunnel failed to open egress tunnel", slog.String("destination", destination), slog.Any("err", err))
			return
		}
		active.bindingMu.Lock()
		if binding.ctx.Err() != nil {
			active.bindingMu.Unlock()
			_ = upstream.Close()
			return
		}
		conn.upstream = upstream
		active.bindingMu.Unlock()

		copyBothWays(downstream, upstream)
	}()
}

func copyBothWays(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		closeWrite(a)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(conn net.Conn) {
	if conn, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = conn.CloseWrite()
	}
}

// EgressPort is the port from a listen address, which each sandbox's redirect
// aims at. The address itself is never bound: egress is served from inside the
// sandbox namespaces.
func EgressPort(listenAddress string) (uint16, error) {
	_, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return 0, fmt.Errorf("atunnel: egress listen address %q: %w", listenAddress, err)
	}
	p, ok := ParsePort(port)
	if !ok {
		return 0, fmt.Errorf("atunnel: egress listen address %q has no usable port", listenAddress)
	}
	return uint16(p), nil
}
