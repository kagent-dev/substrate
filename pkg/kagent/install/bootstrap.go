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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"go.ate.dev/substrate/internal/apiconfig"
	"go.ate.dev/substrate/internal/installdefaults"
	"go.ate.dev/substrate/internal/localca"
	"go.ate.dev/substrate/internal/localjwtauthority"
)

// The secret formats here match cmd/ate-setup/internal/steps/create.go, which
// the charts' READMEs document.

const (
	caValidity      = 365 * 24 * time.Hour
	inClusterIssuer = "https://kubernetes.default.svc"

	rustfsCredentialsSecret = "rustfs-credentials"
	authnConfigMap          = "ate-api-authentication"
)

// caPool names a CA pool Secret.
type caPool struct {
	namespace string
	name      string
	keyType   localca.KeyType
}

var podCertCAPools = []caPool{
	{PodCertNamespace, "service-dns-ca-pool", localca.KeyTypeED25519},
	{PodCertNamespace, "pod-identity-ca-pool", localca.KeyTypeED25519},
	{PodCertNamespace, "postgres-ca-pool", localca.KeyTypeED25519},
}

var (
	actorIDCAPool    = caPool{SystemNamespace, "actor-id-ca-pool", localca.KeyTypeECDSAP256}
	egressMITMCAPool = caPool{SystemNamespace, "egress-mitm-ca-pool", localca.KeyTypeECDSAP256}
)

// createIfAbsent creates s and reports whether it did. An existing Secret is
// kept as is: rotating a CA would break every identity it issued.
func createIfAbsent(ctx context.Context, kc kubernetes.Interface, s *corev1.Secret) (bool, error) {
	_, err := kc.CoreV1().Secrets(s.Namespace).Create(ctx, s, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("while creating secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return true, nil
}

func secretExists(ctx context.Context, kc kubernetes.Interface, namespace, name string) (bool, error) {
	_, err := kc.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("while reading secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

func ensureNamespace(ctx context.Context, kc kubernetes.Interface, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := kc.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("while creating namespace %s: %w", name, err)
	}
	return nil
}

// ensureCAPool creates the pool's Secret unless it exists.
func ensureCAPool(ctx context.Context, kc kubernetes.Interface, p caPool) (bool, error) {
	if ok, err := secretExists(ctx, kc, p.namespace, p.name); ok || err != nil {
		return false, err
	}
	data, err := newCAPoolSecretData("1", p.keyType)
	if err != nil {
		return false, err
	}
	return createIfAbsent(ctx, kc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.namespace, Name: p.name},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
	})
}

// newCAPoolSecretData returns the pool with one new CA, plus the CA's
// certificate and key under the standard TLS keys for consumers that mount
// those directly.
func newCAPoolSecretData(id string, keyType localca.KeyType) (map[string][]byte, error) {
	ca, err := localca.GenerateCA(id, keyType, caValidity)
	if err != nil {
		return nil, fmt.Errorf("while generating CA: %w", err)
	}
	pool, err := localca.Marshal(&localca.ConcretePool{CAs: []*localca.CA{ca}, ActiveForSigning: id})
	if err != nil {
		return nil, fmt.Errorf("while marshaling CA pool: %w", err)
	}
	crt, err := ca.TLSCertificateChainPEM()
	if err != nil {
		return nil, err
	}
	key, err := ca.TLSPrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"pool":                  pool,
		corev1.TLSCertKey:       crt,
		corev1.TLSPrivateKeyKey: key,
	}, nil
}

// poolRootsPEM returns the root certificates of a CA pool Secret.
func poolRootsPEM(ctx context.Context, kc kubernetes.Interface, p caPool) ([]byte, error) {
	s, err := kc.CoreV1().Secrets(p.namespace).Get(ctx, p.name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("while reading CA pool %s/%s: %w", p.namespace, p.name, err)
	}
	pool, err := localca.Unmarshal(s.Data["pool"])
	if err != nil {
		return nil, fmt.Errorf("while decoding CA pool %s/%s: %w", p.namespace, p.name, err)
	}
	if len(pool.CAs) == 0 {
		return nil, fmt.Errorf("CA pool %s/%s has no CAs", p.namespace, p.name)
	}
	var buf bytes.Buffer
	for _, ca := range pool.CAs {
		if ca.RootCertificate == nil {
			return nil, fmt.Errorf("CA %q in pool %s/%s has no root certificate", ca.ID, p.namespace, p.name)
		}
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: ca.RootCertificate.Raw}); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// createdSummary describes which of names a step created.
func createdSummary(created []string, total int) string {
	switch len(created) {
	case 0:
		return "already present"
	case total:
		return "created " + strings.Join(created, ", ")
	default:
		return fmt.Sprintf("created %s; kept %d existing", strings.Join(created, ", "), total-len(created))
	}
}

func (in *Installer) createPodCertCAs(ctx context.Context, _ func(string)) (string, error) {
	if err := ensureNamespace(ctx, in.kube, PodCertNamespace); err != nil {
		return "", err
	}
	var created []string
	for _, p := range podCertCAPools {
		ok, err := ensureCAPool(ctx, in.kube, p)
		if err != nil {
			return "", err
		}
		if ok {
			created = append(created, p.name)
		}
	}
	return createdSummary(created, len(podCertCAPools)), nil
}

func (in *Installer) createIdentitySecrets(ctx context.Context, _ func(string)) (string, error) {
	if err := ensureNamespace(ctx, in.kube, SystemNamespace); err != nil {
		return "", err
	}
	var created []string
	note := func(ok bool, name string) {
		if ok {
			created = append(created, name)
		}
	}

	ok, err := in.ensureJWTPool(ctx)
	if err != nil {
		return "", err
	}
	note(ok, "actor-id-jwt-pool")

	for _, p := range []caPool{actorIDCAPool, egressMITMCAPool} {
		ok, err := ensureCAPool(ctx, in.kube, p)
		if err != nil {
			return "", err
		}
		note(ok, p.name)
	}

	roots, err := poolRootsPEM(ctx, in.kube, actorIDCAPool)
	if err != nil {
		return "", err
	}
	ok, err = createIfAbsent(ctx, in.kube, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "actor-id-ca-certs"},
		Data:       map[string][]byte{"ca.crt": roots},
	})
	if err != nil {
		return "", err
	}
	note(ok, "actor-id-ca-certs")

	ok, err = in.ensureAuthnConfig(ctx)
	if err != nil {
		return "", err
	}
	note(ok, authnConfigMap)

	return createdSummary(created, 5), nil
}

func (in *Installer) ensureJWTPool(ctx context.Context) (bool, error) {
	const name = "actor-id-jwt-pool"
	if ok, err := secretExists(ctx, in.kube, SystemNamespace, name); ok || err != nil {
		return false, err
	}
	pool, _, err := localjwtauthority.GeneratePool("ES256", "")
	if err != nil {
		return false, fmt.Errorf("while generating JWT pool: %w", err)
	}
	return createIfAbsent(ctx, in.kube, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name},
		Data:       map[string][]byte{"pool": pool},
	})
}

// ensureAuthnConfig creates the ate-api-server authentication config. It
// trusts the cluster's service account issuer, which kubectl ate uses.
func (in *Installer) ensureAuthnConfig(ctx context.Context) (bool, error) {
	_, err := in.kube.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, authnConfigMap, metav1.GetOptions{})
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	issuer, err := in.serviceAccountIssuer(ctx)
	if err != nil {
		return false, err
	}
	cfg := &apiconfig.AuthenticationConfig{
		JWTProviders: []apiconfig.JWTProviderConfig{kubernetesJWTProvider(issuer)},
	}
	if err := apiconfig.ValidateAuthenticationConfig(cfg); err != nil {
		return false, err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return false, err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: authnConfigMap},
		Data:       map[string]string{"authentication.yaml": string(b)},
	}
	_, err = in.kube.CoreV1().ConfigMaps(SystemNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("while creating configmap %s: %w", authnConfigMap, err)
	}
	return true, nil
}

// serviceAccountIssuer reads the issuer from the API server's OIDC discovery
// document, or returns the in-cluster default when it isn't published.
func (in *Installer) serviceAccountIssuer(ctx context.Context) (string, error) {
	rc := in.kube.Discovery().RESTClient()
	if rc == nil {
		return inClusterIssuer, nil
	}
	raw, err := rc.Get().AbsPath("/.well-known/openid-configuration").DoRaw(ctx)
	if apierrors.IsNotFound(err) {
		return inClusterIssuer, nil
	}
	if err != nil {
		return "", fmt.Errorf("while reading the service account issuer from /.well-known/openid-configuration: %w", err)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("while decoding /.well-known/openid-configuration: %w", err)
	}
	if doc.Issuer == "" {
		return inClusterIssuer, nil
	}
	return doc.Issuer, nil
}

// kubernetesJWTProvider points an in-cluster issuer at the service account
// CA and token, since it isn't reachable over public discovery.
func kubernetesJWTProvider(issuer string) apiconfig.JWTProviderConfig {
	p := apiconfig.JWTProviderConfig{
		Name:      "kubernetes",
		Issuer:    issuer,
		Audiences: []string{installdefaults.APIServiceHost(SystemNamespace)},
	}
	switch issuer {
	case inClusterIssuer, inClusterIssuer + ".cluster.local":
		p.CertificateAuthorityFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
		p.DiscoveryTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	return p
}

// configureObjectStore creates random credentials for the bundled RustFS, or
// checks the values an external store needs.
func (in *Installer) configureObjectStore(ctx context.Context, _ func(string)) (string, error) {
	if err := ensureNamespace(ctx, in.kube, SystemNamespace); err != nil {
		return "", err
	}
	if in.opts.ObjectStore.Type != ObjectStoreBundled {
		// A resumed install keeps the values the releases already have.
		atelet, substrate := in.externalStoreValues()
		vals := map[release]map[string]any{}
		for r, typed := range map[release]map[string]any{ateletRelease: atelet, substrateRelease: substrate} {
			rel, err := in.helm.get(r)
			if err != nil {
				return "", err
			}
			var stored map[string]any
			if rel != nil {
				stored = rel.Config
			}
			vals[r] = mergeValues(stored, in.user[r.name], typed)
		}
		return in.checkExternalObjectStore(ctx, vals[substrateRelease], vals[ateletRelease])
	}
	if ok, err := secretExists(ctx, in.kube, SystemNamespace, rustfsCredentialsSecret); ok || err != nil {
		return "already present", err
	}
	ok, err := createIfAbsent(ctx, in.kube, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: rustfsCredentialsSecret},
		StringData: map[string]string{
			"AWS_ACCESS_KEY_ID":     "substrate-" + randomHex(8),
			"AWS_SECRET_ACCESS_KEY": randomHex(32),
		},
	})
	if err != nil {
		return "", err
	}
	if !ok {
		return "already present", nil
	}
	return "created " + rustfsCredentialsSecret, nil
}

// checkExternalObjectStore fails before any release changes when a value the
// charts need for an external store is missing from the substrate or atelet
// release values.
func (in *Installer) checkExternalObjectStore(ctx context.Context, substrate, atelet map[string]any) (string, error) {
	location, _ := lookupString(substrate, "ateApi", "snapshotLocation")
	if location == "" && !previewEnabled(substrate, "PerTemplateSnapshotLocation") {
		return "", errors.New("set objectStore.bucket to the snapshot bucket")
	}
	if in.opts.ObjectStore.Type == ObjectStoreGCS {
		if sa, _ := lookupString(substrate, "objectStorePlugin", "transferServiceAccount"); sa == "" {
			return "", errors.New("set objectStore.gcs.transferServiceAccount to the Google service account for snapshot transfers")
		}
	}
	if name, _ := lookupString(atelet, "objectStore", "credentialsSecret"); name != "" {
		ok, err := secretExists(ctx, in.kube, SystemNamespace, name)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("objectStore.credentialsSecret %s/%s doesn't exist", SystemNamespace, name)
		}
	}
	if location == "" {
		return string(in.opts.ObjectStore.Type) + ", per-template locations", nil
	}
	return string(in.opts.ObjectStore.Type) + " " + location, nil
}

// lookup returns the value at path in vals, or nil.
func lookup(vals any, path ...string) any {
	for _, k := range path {
		m, ok := vals.(map[string]any)
		if !ok {
			return nil
		}
		vals = m[k]
	}
	return vals
}

// lookupString returns the string at path in vals.
func lookupString(vals map[string]any, path ...string) (string, bool) {
	s, ok := lookup(vals, path...).(string)
	return s, ok
}

// previewEnabled reports whether the substrate chart's preview list enables
// gate, directly or through ALL.
func previewEnabled(vals map[string]any, gate string) bool {
	list, _ := vals["preview"].([]any)
	for _, v := range list {
		if v == gate || v == "ALL" {
			return true
		}
	}
	return false
}

// rustfsCredentials reads the credentials configureObjectStore created.
func (in *Installer) rustfsCredentials(ctx context.Context) (accessKey, secretKey string, err error) {
	s, err := in.kube.CoreV1().Secrets(SystemNamespace).Get(ctx, rustfsCredentialsSecret, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("while reading %s: %w", rustfsCredentialsSecret, err)
	}
	accessKey, secretKey = string(s.Data["AWS_ACCESS_KEY_ID"]), string(s.Data["AWS_SECRET_ACCESS_KEY"])
	if accessKey == "" || secretKey == "" {
		return "", "", fmt.Errorf("secret %s lacks AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY", rustfsCredentialsSecret)
	}
	return accessKey, secretKey, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error.
	return hex.EncodeToString(b)
}
