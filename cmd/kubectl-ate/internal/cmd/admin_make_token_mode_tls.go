// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

var tokenTLSNamespace, tokenTLSCASecret, tokenTLSSecret, tokenTLSCAConfigMap string
var tokenTLSDNSNames []string

var makeTokenModeTLSCmd = &cobra.Command{
	Use:   "make-token-mode-tls",
	Short: "Create the shared TLS identity used by token-mode workloads",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := ateclient.LoadConfig(kubeconfig, k8sContext)
		if err != nil {
			return fmt.Errorf("while reading kubeconfig: %w", err)
		}
		kc, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return fmt.Errorf("while creating Kubernetes client: %w", err)
		}
		return createTokenModeTLS(cmd.Context(), kc)
	},
}

func createTokenModeTLS(ctx context.Context, kc kubernetes.Interface) error {
	secrets := kc.CoreV1().Secrets(tokenTLSNamespace)
	if _, err := secrets.Get(ctx, tokenTLSSecret, metav1.GetOptions{}); err == nil {
		secret, err := secrets.Get(ctx, tokenTLSCASecret, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("TLS secret exists but CA secret is unavailable: %w", err)
		}
		pool, err := localca.Unmarshal(secret.Data["pool"])
		if err != nil || len(pool.CAs) != 1 {
			return fmt.Errorf("invalid CA secret %q", tokenTLSCASecret)
		}
		if err := ensureTokenModeCAConfigMap(ctx, kc, pool.CAs[0]); err != nil {
			return err
		}
		fmt.Printf("TLS secret %s/%s already exists; leaving it unchanged\n", tokenTLSNamespace, tokenTLSSecret)
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("while checking TLS secret: %w", err)
	}

	ca, err := loadOrCreateTokenModeCA(ctx, secrets)
	if err != nil {
		return err
	}
	if err := ensureTokenModeCAConfigMap(ctx, kc, ca); err != nil {
		return err
	}
	leafCert, leafKey, err := issueTokenModeLeaf(ca, tokenTLSDNSNames)
	if err != nil {
		return err
	}
	credentialBundle := append(append([]byte{}, leafCert...), leafKey...)
	if _, err := secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenTLSSecret, Namespace: tokenTLSNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       leafCert,
			corev1.TLSPrivateKeyKey: leafKey,
			"credential-bundle.pem": credentialBundle,
		},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("while creating TLS secret: %w", err)
	}

	fmt.Printf("Successfully created shared token-mode TLS secret %s/%s\n", tokenTLSNamespace, tokenTLSSecret)
	return nil
}

func ensureTokenModeCAConfigMap(ctx context.Context, kc kubernetes.Interface, ca *localca.CA) error {
	configMaps := kc.CoreV1().ConfigMaps(tokenTLSNamespace)
	_, err := configMaps.Get(ctx, tokenTLSCAConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.RootCertificate.Raw}))
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: tokenTLSCAConfigMap, Namespace: tokenTLSNamespace},
			Data:       map[string]string{"ca.crt": caPEM, "trust-bundle.pem": caPEM},
		}, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("while creating CA ConfigMap: %w", err)
	}
	return nil
}

func loadOrCreateTokenModeCA(ctx context.Context, secrets corev1client.SecretInterface) (*localca.CA, error) {
	secret, err := secrets.Get(ctx, tokenTLSCASecret, metav1.GetOptions{})
	if err == nil {
		pool, err := localca.Unmarshal(secret.Data["pool"])
		if err != nil || len(pool.CAs) != 1 {
			return nil, fmt.Errorf("invalid CA secret %q", tokenTLSCASecret)
		}
		return pool.CAs[0], nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("while reading CA secret: %w", err)
	}
	ca, err := localca.GenerateED25519CA("token-mode")
	if err != nil {
		return nil, err
	}
	pool, err := localca.Marshal(&localca.Pool{CAs: []*localca.CA{ca}})
	if err != nil {
		return nil, err
	}
	if _, err := secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenTLSCASecret, Namespace: tokenTLSNamespace},
		Data:       map[string][]byte{"pool": pool},
	}, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("while creating CA secret: %w", err)
	}
	return ca, nil
}

func issueTokenModeLeaf(ca *localca.CA, dnsNames []string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "agent-substrate-token-mode"},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.RootCertificate, &key.PublicKey, ca.SigningKey)
	if err != nil {
		return nil, nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

func init() {
	adminCmd.AddCommand(makeTokenModeTLSCmd)
	makeTokenModeTLSCmd.Flags().StringVar(&tokenTLSNamespace, "namespace", "ate-system", "Namespace for generated resources")
	makeTokenModeTLSCmd.Flags().StringVar(&tokenTLSCASecret, "ca-secret", "token-mode-ca", "Secret holding the signing CA")
	makeTokenModeTLSCmd.Flags().StringVar(&tokenTLSSecret, "tls-secret", "token-mode-tls", "Secret holding the shared leaf identity")
	makeTokenModeTLSCmd.Flags().StringVar(&tokenTLSCAConfigMap, "ca-configmap", "token-mode-ca", "ConfigMap holding the public CA")
	makeTokenModeTLSCmd.Flags().StringSliceVar(&tokenTLSDNSNames, "dns-names", nil, "DNS SANs for the shared leaf certificate")
	makeTokenModeTLSCmd.MarkFlagRequired("dns-names")
}
