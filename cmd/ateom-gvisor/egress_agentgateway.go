//go:build linux

//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

const (
	egressAgentgatewayHTTPPort  = 15001
	egressAgentgatewayHTTPSPort = 15002
)

type egressAgentgatewayManager struct {
	lock       sync.Mutex
	binaryPath string
	namespace  string
	podName    string
	configPath string
	tlsDir     string
	secret     egressSecretResolver
	cmd        *exec.Cmd
}

type egressSecretResolver interface {
	SecretValue(ctx context.Context, namespace, name, key string) (string, error)
	TLSSecret(ctx context.Context, namespace, name string) ([]byte, []byte, error)
}

func newEgressAgentgatewayManager(ctx context.Context, binaryPath, namespace, podName string) (*egressAgentgatewayManager, error) {
	manager := &egressAgentgatewayManager{
		binaryPath: binaryPath,
		namespace:  namespace,
		podName:    podName,
		configPath: ateompath.EgressAgentgatewayConfigPath(namespace, podName),
		tlsDir:     ateompath.EgressAgentgatewayTLSDir(namespace, podName),
	}
	secret, err := newKubernetesEgressSecretResolver()
	if err != nil {
		slog.InfoContext(ctx, "Kubernetes secret resolver unavailable for egress agentgateway", slog.Any("err", err))
	} else {
		manager.secret = secret
	}
	if err := manager.writeConfig(ctx, nil); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *egressAgentgatewayManager) ApplyPolicy(ctx context.Context, policy *ateompb.EgressPolicy) error {
	if m == nil {
		return nil
	}
	m.lock.Lock()
	defer m.lock.Unlock()

	if err := m.writeConfig(ctx, policy); err != nil {
		return err
	}
	if !egressPolicyNeedsAgentgateway(policy) {
		return m.stopLocked(ctx)
	}
	if m.cmd != nil && m.cmd.Process != nil {
		return nil
	}

	reapLock.RLock()
	cmd := exec.Command(m.binaryPath, "-f", m.configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		reapLock.RUnlock()
		return fmt.Errorf("while starting egress agentgateway %q: %w", m.binaryPath, err)
	}
	reapLock.RUnlock()

	m.cmd = cmd
	go func() {
		err := cmd.Wait()
		m.lock.Lock()
		if m.cmd == cmd {
			m.cmd = nil
		}
		m.lock.Unlock()
		if err != nil {
			slog.WarnContext(ctx, "Egress agentgateway exited", slog.Any("err", err))
		}
	}()
	slog.InfoContext(ctx, "Started egress agentgateway", slog.String("config", m.configPath))
	return nil
}

func (m *egressAgentgatewayManager) Stop(ctx context.Context) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if err := m.stopLocked(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to stop egress agentgateway", slog.Any("err", err))
	}
}

func (m *egressAgentgatewayManager) stopLocked(ctx context.Context) error {
	if m.cmd == nil || m.cmd.Process == nil {
		return nil
	}
	slog.InfoContext(ctx, "Stopping egress agentgateway")
	if err := m.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("while killing egress agentgateway: %w", err)
	}
	m.cmd = nil
	return nil
}

func (m *egressAgentgatewayManager) writeConfig(ctx context.Context, policy *ateompb.EgressPolicy) error {
	if err := os.MkdirAll(filepath.Dir(m.configPath), 0o700); err != nil {
		return fmt.Errorf("while creating egress agentgateway config directory: %w", err)
	}
	if err := os.MkdirAll(m.tlsDir, 0o700); err != nil {
		return fmt.Errorf("while creating egress agentgateway tls directory: %w", err)
	}
	cfg, err := renderEgressAgentgatewayConfig(ctx, m.secret, m.namespace, m.tlsDir, policy)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.configPath, cfg, 0o600); err != nil {
		return fmt.Errorf("while writing egress agentgateway config: %w", err)
	}
	return nil
}

func egressPolicyNeedsAgentgateway(policy *ateompb.EgressPolicy) bool {
	if policy == nil {
		return false
	}
	for _, rule := range policy.GetAllow() {
		if egressRuleHasTransparentHTTPRoute(rule) {
			return true
		}
		if len(rule.GetCredentials().GetInject()) > 0 {
			return true
		}
		switch rule.GetTls().GetMode() {
		case "Require", "Originate", "Intercept":
			return true
		}
		if rule.GetTls().GetRequired() {
			return true
		}
	}
	return false
}

func egressRuleHasTransparentHTTPRoute(rule *ateompb.EgressPolicyRule) bool {
	hasHost := false
	for _, dest := range rule.GetTo() {
		if dest.GetHost() != "" {
			hasHost = true
			break
		}
	}
	if !hasHost || egressRuleRequiresBackendTLS(rule) {
		return false
	}
	ports := rule.GetPorts()
	if len(ports) == 0 {
		return true
	}
	for _, port := range ports {
		if isTCPEgressPort(port) && effectiveEgressPort(port) == 80 {
			return true
		}
	}
	return false
}

type localAgentgatewayConfig struct {
	Schema string                    `json:"# yaml-language-server,omitempty"`
	Config localAgentgatewaySettings `json:"config"`
	Binds  []localAgentgatewayBind   `json:"binds"`
}

type localAgentgatewaySettings struct {
	AdminAddr     string `json:"adminAddr"`
	ReadinessAddr string `json:"readinessAddr"`
	StatsAddr     string `json:"statsAddr"`
}

type localAgentgatewayBind struct {
	Port      int                         `json:"port"`
	Listeners []localAgentgatewayListener `json:"listeners"`
}

type localAgentgatewayListener struct {
	Name      string                        `json:"name"`
	Protocol  string                        `json:"protocol"`
	TLS       *localAgentgatewayFrontendTLS `json:"tls,omitempty"`
	Routes    []localAgentgatewayRoute      `json:"routes,omitempty"`
	TCPRoutes []localAgentgatewayTCPRoute   `json:"tcpRoutes,omitempty"`
}

type localAgentgatewayFrontendTLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

type localAgentgatewayRoute struct {
	Name     string                          `json:"name"`
	Matches  []localAgentgatewayMatch        `json:"matches,omitempty"`
	Policies *localAgentgatewayRoutePolicy   `json:"policies,omitempty"`
	Backends []localAgentgatewayRouteBackend `json:"backends,omitempty"`
}

type localAgentgatewayMatch struct {
	Headers []localAgentgatewayHeaderMatch `json:"headers,omitempty"`
}

type localAgentgatewayHeaderMatch struct {
	Name  string                            `json:"name"`
	Value localAgentgatewayStringMatchValue `json:"value"`
}

type localAgentgatewayStringMatchValue struct {
	Exact string `json:"exact"`
}

type localAgentgatewayRoutePolicy struct {
	RequestHeaderModifier *localAgentgatewayHeaderModifier `json:"requestHeaderModifier,omitempty"`
	DirectResponse        *localAgentgatewayDirectResponse `json:"directResponse,omitempty"`
}

type localAgentgatewayHeaderModifier struct {
	Set map[string]string `json:"set,omitempty"`
}

type localAgentgatewayDirectResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

type localAgentgatewayRouteBackend struct {
	Host     string                            `json:"host"`
	Policies *localAgentgatewayBackendPolicies `json:"policies,omitempty"`
}

type localAgentgatewayTCPRoute struct {
	Name      string                          `json:"name"`
	Hostnames []string                        `json:"hostnames,omitempty"`
	Backends  []localAgentgatewayRouteBackend `json:"backends,omitempty"`
}

type localAgentgatewayBackendPolicies struct {
	BackendTLS map[string]any `json:"backendTLS,omitempty"`
}

func renderEgressAgentgatewayConfig(ctx context.Context, secrets egressSecretResolver, defaultNamespace, tlsDir string, policy *ateompb.EgressPolicy) ([]byte, error) {
	cfg := localAgentgatewayConfig{
		Config: localAgentgatewaySettings{
			AdminAddr:     "127.0.0.1:15000",
			ReadinessAddr: "127.0.0.1:15021",
			StatsAddr:     "127.0.0.1:15020",
		},
		Binds: []localAgentgatewayBind{
			{
				Port: egressAgentgatewayHTTPPort,
				Listeners: []localAgentgatewayListener{{
					Name:     "egress-http",
					Protocol: "HTTP",
					Routes:   []localAgentgatewayRoute{denyEgressRoute()},
				}},
			},
		},
	}

	httpsListener := localAgentgatewayListener{
		Name:     "egress-https",
		Protocol: "TLS",
	}
	for _, rule := range policy.GetAllow() {
		routes, err := routesForEgressRule(ctx, secrets, defaultNamespace, tlsDir, rule)
		if err != nil {
			return nil, err
		}
		for _, route := range routes {
			if rule.GetTls().GetMode() == "Intercept" {
				httpsListener.Protocol = "HTTPS"
				if httpsListener.TLS == nil {
					tls, err := frontendTLSForInterceptRule(ctx, secrets, defaultNamespace, tlsDir, rule)
					if err != nil {
						return nil, err
					}
					httpsListener.TLS = tls
				}
				httpsListener.Routes = append([]localAgentgatewayRoute{route}, httpsListener.Routes...)
				continue
			}
			if egressRuleRequiresBackendTLS(rule) {
				continue
			}
			cfg.Binds[0].Listeners[0].Routes = append([]localAgentgatewayRoute{route}, cfg.Binds[0].Listeners[0].Routes...)
		}
		if httpsListener.Protocol == "TLS" && egressRuleRequiresBackendTLS(rule) {
			httpsListener.TCPRoutes = append(httpsListener.TCPRoutes, tcpRoutesForEgressRule(rule)...)
		}
	}
	if httpsListener.TLS != nil || len(httpsListener.TCPRoutes) > 0 {
		if httpsListener.TLS != nil && len(httpsListener.Routes) == 0 {
			httpsListener.Routes = []localAgentgatewayRoute{denyEgressRoute()}
		}
		cfg.Binds = append(cfg.Binds, localAgentgatewayBind{
			Port:      egressAgentgatewayHTTPSPort,
			Listeners: []localAgentgatewayListener{httpsListener},
		})
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("while rendering egress agentgateway config: %w", err)
	}
	return append([]byte("# yaml-language-server: $schema=https://agentgateway.dev/schema/config\n"), out...), nil
}

func routesForEgressRule(ctx context.Context, secrets egressSecretResolver, defaultNamespace, _ string, rule *ateompb.EgressPolicyRule) ([]localAgentgatewayRoute, error) {
	var routes []localAgentgatewayRoute
	for _, dest := range rule.GetTo() {
		host := dest.GetHost()
		if host == "" {
			continue
		}
		ports := rule.GetPorts()
		if len(ports) == 0 {
			ports = []*ateompb.EgressPort{{Port: 443, Protocol: "TCP"}}
		}
		for _, port := range ports {
			if !isTCPEgressPort(port) {
				continue
			}
			backend := localAgentgatewayRouteBackend{Host: fmt.Sprintf("%s:%d", host, effectiveEgressPort(port))}
			if egressRuleRequiresBackendTLS(rule) {
				backend.Policies = &localAgentgatewayBackendPolicies{BackendTLS: map[string]any{}}
			}
			route := localAgentgatewayRoute{
				Name:     localRouteName(host, port),
				Matches:  egressAuthorityMatches(host, effectiveEgressPort(port)),
				Backends: []localAgentgatewayRouteBackend{backend},
			}
			headers, err := egressInjectedHeaders(ctx, secrets, defaultNamespace, rule.GetCredentials())
			if err != nil {
				return nil, err
			}
			if len(headers) > 0 {
				route.Policies = &localAgentgatewayRoutePolicy{
					RequestHeaderModifier: &localAgentgatewayHeaderModifier{Set: headers},
				}
			}
			routes = append(routes, route)
		}
	}
	return routes, nil
}

func tcpRoutesForEgressRule(rule *ateompb.EgressPolicyRule) []localAgentgatewayTCPRoute {
	var routes []localAgentgatewayTCPRoute
	for _, dest := range rule.GetTo() {
		host := dest.GetHost()
		if host == "" {
			continue
		}
		ports := rule.GetPorts()
		if len(ports) == 0 {
			ports = []*ateompb.EgressPort{{Port: 443, Protocol: "TCP"}}
		}
		for _, port := range ports {
			if !isTCPEgressPort(port) || effectiveEgressPort(port) != 443 {
				continue
			}
			routes = append(routes, localAgentgatewayTCPRoute{
				Name:      localRouteName(host, port),
				Hostnames: []string{host},
				Backends: []localAgentgatewayRouteBackend{{
					Host: fmt.Sprintf("%s:%d", host, effectiveEgressPort(port)),
				}},
			})
		}
	}
	return routes
}

func egressAuthorityMatches(host string, port uint32) []localAgentgatewayMatch {
	matches := []localAgentgatewayMatch{
		{Headers: []localAgentgatewayHeaderMatch{{
			Name:  ":authority",
			Value: localAgentgatewayStringMatchValue{Exact: host},
		}}},
	}
	hostWithPort := fmt.Sprintf("%s:%d", host, port)
	if hostWithPort != host {
		matches = append(matches, localAgentgatewayMatch{Headers: []localAgentgatewayHeaderMatch{{
			Name:  ":authority",
			Value: localAgentgatewayStringMatchValue{Exact: hostWithPort},
		}}})
	}
	return matches
}

func egressInjectedHeaders(ctx context.Context, secrets egressSecretResolver, defaultNamespace string, policy *ateompb.EgressCredentialPolicy) (map[string]string, error) {
	if policy == nil || len(policy.GetInject()) == 0 {
		return nil, nil
	}
	if secrets == nil {
		return nil, fmt.Errorf("egress credential injection requires Kubernetes secret access")
	}
	headers := map[string]string{}
	for _, injection := range policy.GetInject() {
		header := strings.TrimSpace(injection.GetHeader())
		if header == "" {
			return nil, fmt.Errorf("egress credential injection header is required")
		}
		ref := injection.GetValueFrom().GetSecretKeyRef()
		if ref == nil {
			return nil, fmt.Errorf("egress credential injection for header %q requires valueFrom.secretKeyRef", header)
		}
		value, err := secrets.SecretValue(ctx, defaultNamespace, ref.GetName(), ref.GetKey())
		if err != nil {
			return nil, err
		}
		headers[header] = value
	}
	return headers, nil
}

func frontendTLSForInterceptRule(ctx context.Context, secrets egressSecretResolver, defaultNamespace, tlsDir string, rule *ateompb.EgressPolicyRule) (*localAgentgatewayFrontendTLS, error) {
	if secrets == nil {
		return nil, fmt.Errorf("egress TLS intercept requires Kubernetes secret access")
	}
	ref := rule.GetTls().GetIntercept().GetIssuerSecretRef()
	if ref == nil {
		return nil, fmt.Errorf("egress TLS intercept requires tls.intercept.issuerSecretRef")
	}
	namespace := ref.GetNamespace()
	if namespace == "" {
		namespace = defaultNamespace
	}
	cert, key, err := secrets.TLSSecret(ctx, namespace, ref.GetName())
	if err != nil {
		return nil, err
	}
	certPath := filepath.Join(tlsDir, "intercept.crt")
	keyPath := filepath.Join(tlsDir, "intercept.key")
	if err := os.WriteFile(certPath, cert, 0o600); err != nil {
		return nil, fmt.Errorf("while writing egress intercept cert: %w", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("while writing egress intercept key: %w", err)
	}
	return &localAgentgatewayFrontendTLS{Cert: certPath, Key: keyPath}, nil
}

func denyEgressRoute() localAgentgatewayRoute {
	return localAgentgatewayRoute{
		Name: "default-deny",
		Policies: &localAgentgatewayRoutePolicy{
			DirectResponse: &localAgentgatewayDirectResponse{
				Status: 403,
				Body:   "egress denied",
			},
		},
	}
}

func egressRuleRequiresBackendTLS(rule *ateompb.EgressPolicyRule) bool {
	tls := rule.GetTls()
	return tls.GetRequired() || tls.GetMode() == "Require" || tls.GetMode() == "Originate" || tls.GetMode() == "Intercept"
}

func isTCPEgressPort(port *ateompb.EgressPort) bool {
	return port.GetProtocol() == "" || strings.EqualFold(port.GetProtocol(), "TCP")
}

func effectiveEgressPort(port *ateompb.EgressPort) uint32 {
	if port.GetPort() == 0 {
		return 443
	}
	return port.GetPort()
}

func localRouteName(host string, port *ateompb.EgressPort) string {
	base := host
	base = strings.NewReplacer(".", "-", "_", "-", ":", "-").Replace(base)
	return fmt.Sprintf("allow-%s-%d", base, effectiveEgressPort(port))
}

type kubernetesEgressSecretResolver struct {
	client kubernetes.Interface
}

func newKubernetesEgressSecretResolver() (*kubernetesEgressSecretResolver, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kubernetesEgressSecretResolver{client: client}, nil
}

func (r *kubernetesEgressSecretResolver) SecretValue(ctx context.Context, defaultNamespace, name, key string) (string, error) {
	if name == "" || key == "" {
		return "", fmt.Errorf("egress credential secret name and key are required")
	}
	secret, err := r.client.CoreV1().Secrets(defaultNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", secretGetError(defaultNamespace, name, err)
	}
	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s does not contain key %q", defaultNamespace, name, key)
	}
	return string(value), nil
}

func (r *kubernetesEgressSecretResolver) TLSSecret(ctx context.Context, namespace, name string) ([]byte, []byte, error) {
	if namespace == "" {
		return nil, nil, fmt.Errorf("egress TLS intercept secret namespace is required")
	}
	if name == "" {
		return nil, nil, fmt.Errorf("egress TLS intercept secret name is required")
	}
	secret, err := r.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, secretGetError(namespace, name, err)
	}
	cert := secret.Data[corev1.TLSCertKey]
	key := secret.Data[corev1.TLSPrivateKeyKey]
	if len(cert) == 0 || len(key) == 0 {
		return nil, nil, fmt.Errorf("secret %s/%s must contain %q and %q for egress TLS intercept", namespace, name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}
	return cert, key, nil
}

func secretGetError(namespace, name string, err error) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("ateom service account cannot read secret %s/%s for egress agentgateway config: %w", namespace, name, err)
	}
	return fmt.Errorf("while reading secret %s/%s for egress agentgateway config: %w", namespace, name, err)
}
