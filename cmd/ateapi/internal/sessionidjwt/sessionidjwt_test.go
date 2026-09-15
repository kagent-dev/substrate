package sessionidjwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testClaims(custom map[string]string) *Claims {
	return &Claims{
		Issuer:     "https://id.ai.videoamp.tools",
		Subject:    "apps/test-app/users/research/sessions/probe-1",
		Audiences:  []string{"sts.amazonaws.com"},
		Expiration: time.Unix(1800000000, 0),
		NotBefore:  time.Unix(1700000000, 0),
		IssuedAt:   time.Unix(1700000000, 0),
		JTI:        "test-jti",
		Substrate:  SubstrateClaims{AppID: "test-app", UserID: "research", SessionID: "probe-1"},
		Custom:     custom,
	}
}

func decodePayload(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 jwt segments, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}

func TestCustomClaimsFlatten(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := ClaimsToWire(testClaims(map[string]string{"org_id": "acme-123"}))
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := Sign(wire, key, "ES256", "1")
	if err != nil {
		t.Fatal(err)
	}
	payload := decodePayload(t, jwt)
	if got := payload["org_id"]; got != "acme-123" {
		t.Fatalf("org_id = %v, want acme-123", got)
	}
	if got := payload["iss"]; got != "https://id.ai.videoamp.tools" {
		t.Fatalf("iss = %v", got)
	}
	substrate, ok := payload["ate.dev"].(map[string]any)
	if !ok || substrate["appID"] != "test-app" {
		t.Fatalf("ate.dev claims mangled: %v", payload["ate.dev"])
	}
}

func TestCustomClaimsCollisionRejected(t *testing.T) {
	wire, err := ClaimsToWire(testClaims(map[string]string{"iss": "https://evil.example"}))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(wire, key, "ES256", "1"); err == nil {
		t.Fatal("expected collision error, got nil")
	}
}
