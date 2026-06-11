# agentgateway Egress

This guide covers the local development path for running Substrate with
agentgateway bundled into the `ateom-gvisor` image for actor egress.

## Setup

1. Install Substrate with agentgateway egress bundling enabled:

   ```sh
   export KO_DOCKER_REPO=localhost:5001

   ./hack/install-ate-kind.sh --bundle-agentgateway-egress=enable --deploy-ate-system --router=agentgateway
   ./hack/install-ate-kind.sh --bundle-agentgateway-egress=enable --deploy-demo-counter
   ```

2. Install the `kubectl ate` plugin:

   ```sh
   go install ./cmd/kubectl-ate
   ```

   If `kubectl ate` is still unavailable, make sure your Go bin directory is in
   `PATH`, for example `export PATH="$(go env GOPATH)/bin:$PATH"`.

The `--bundle-agentgateway-egress=enable` flag configures `ko` for this setup
run so `github.com/agent-substrate/substrate/cmd/ateom-gvisor` uses
`cr.agentgateway.dev/agentgateway:latest-dev` as its base image. Normal
installs without this flag continue to use the default `ko` image settings.

Bundling selects agentgateway as the local egress gateway implementation by
making the `agentgateway` binary available in the `ateom-gvisor` image.
Per-actor enforcement is driven by `ActorTemplate.spec.egressPolicy`: when a
workload has an egress policy that requires local gateway handling,
`ateom-gvisor` starts and configures the bundled egress gateway for that
workload.

The egress policy should describe traffic policy, not the proxy implementation.
For Helm-based installs, the default egress gateway implementation should be a
chart/install setting rather than an `ActorTemplate` field.

To test a different agentgateway image while keeping the same setup path, pass
an explicit image:

```sh
./hack/install-ate-kind.sh \
  --bundle-agentgateway-egress=enable \
  --agentgateway-egress-image=cr.agentgateway.dev/agentgateway:<tag> \
  --deploy-ate-system
```

## Smoke Test

If you changed the demo counter locally, rerun
`./hack/install-ate-kind.sh --bundle-agentgateway-egress=enable
--deploy-demo-counter` before creating the actor so the WorkerPool image still
contains `agentgateway` and the actor snapshot uses the updated counter image.
Use a fresh actor name after changing the template, because existing actors keep
using their previous snapshot.

Reapply the egress policy after every `--deploy-demo-counter` run. The demo
install reapplies the `ActorTemplate`, which clears local patches.

Configure the demo `ActorTemplate` with an egress policy that allows
`example.com`, then denies other HTTP/HTTPS egress. DNS is allowed by default
for name resolution.

```sh
export KO_DOCKER_REPO=localhost:5001

./hack/run-tool.sh ko apply -f - <<EOF
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: counter
  namespace: ate-demo-counter
spec:
  containers:
  - name: counter
    image: ko://github.com/agent-substrate/substrate/demos/counter
    command:
    - /ko-app/counter
  egressPolicy:
    defaultAction: Deny
    allow:
    - name: example
      to:
      - host: example.com
      ports:
      - port: 80
        protocol: TCP
      - port: 443
        protocol: TCP
    audit:
      logs: true
  pauseImage: registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
  runsc:
    amd64:
      url: gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc
      sha256Hash: a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63
    arm64:
      url: gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc
      sha256Hash: 1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9
  snapshotsConfig:
    location: gs://ate-snapshots/ate-demo-counter/
  workerPoolRef:
    name: counter
    namespace: ate-demo-counter
EOF
```

Verify the policy is present before creating the actor:

```sh
kubectl -n ate-demo-counter get actortemplate counter \
  -o jsonpath='{.spec.egressPolicy.defaultAction}{"\n"}'
```

The command should print `Deny`.

Create an actor and port-forward the router:

```sh
kubectl ate create actor my-counter-egress-example --template=ate-demo-counter/counter
kubectl port-forward -n ate-system svc/atenet-router 8000:80
```

Allowed egress to `example.com` should return `200`:

```sh
curl -i \
  -H "Host: my-counter-egress-example.actors.resources.substrate.ate.dev" \
  "http://localhost:8000/egress?url=https://example.com/"
```

Requests outside the policy should fail instead of returning the upstream
response:

```sh
curl -i \
  -H "Host: my-counter-egress-example.actors.resources.substrate.ate.dev" \
  "http://localhost:8000/egress?url=https://www.google.com/generate_204"
```

Check the worker logs for egress policy activity:

```sh
kubectl logs -n ate-demo-counter \
  -l ate.dev/worker-pool=counter \
  -c ateom \
  --tail=200
```

## TLS Intercept

For MITM-style HTTPS inspection, set a rule's TLS mode to `Intercept` and
provide `issuerSecretRef`. The referenced secret is treated as a CA issuer, not
as the final server certificate. agentgateway reads the downstream TLS SNI, such
as `api.openai.com`, dynamically creates a short-lived leaf certificate for that
hostname, signs it with the issuer, and presents it to the actor.

Create a CA secret:

```sh
cat >/tmp/example-mitm-openssl.cnf <<'EOF'
[req]
distinguished_name = dn
prompt = no
x509_extensions = v3_ca

[dn]
CN = example-egress-ca

[v3_ca]
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always,issuer
EOF

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout /tmp/example-mitm.key \
  -out /tmp/example-mitm.crt \
  -days 7 \
  -config /tmp/example-mitm-openssl.cnf

kubectl -n ate-demo-counter create secret tls example-mitm \
  --cert=/tmp/example-mitm.crt \
  --key=/tmp/example-mitm.key \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n ate-demo-counter create role ateom-egress-secret-reader \
  --verb=get \
  --resource=secrets \
  --resource-name=example-mitm \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n ate-demo-counter create rolebinding ateom-egress-secret-reader \
  --role=ateom-egress-secret-reader \
  --serviceaccount=ate-demo-counter:default \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n ate-demo-counter create rolebinding ate-api-egress-secret-reader \
  --role=ateom-egress-secret-reader \
  --serviceaccount=ate-system:ate-api-server \
  --dry-run=client -o yaml | kubectl apply -f -
```

The worker pod service account needs this secret for `issuerSecretRef`.
`ate-api-server` also needs it in this example because it resolves the counter
container's `EGRESS_CA_CERT_PEM` `secretKeyRef` before resuming the actor.

Apply the `ActorTemplate` with an intercept rule and pass the CA certificate
into the counter actor:

```sh
export KO_DOCKER_REPO=localhost:5001

./hack/run-tool.sh ko apply -f - <<EOF
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: counter
  namespace: ate-demo-counter
spec:
  containers:
  - name: counter
    image: ko://github.com/agent-substrate/substrate/demos/counter
    command:
    - /ko-app/counter
    env:
    - name: EGRESS_CA_CERT_PEM
      valueFrom:
        secretKeyRef:
          name: example-mitm
          key: tls.crt
  egressPolicy:
    defaultAction: Deny
    allow:
    - name: example-https-intercept
      to:
      - host: example.com
      ports:
      - port: 443
        protocol: TCP
      tls:
        mode: Intercept
        required: true
        intercept:
          issuerSecretRef:
            name: example-mitm
            namespace: ate-demo-counter
          validateUpstream: true
    audit:
      logs: true
  pauseImage: registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
  runsc:
    amd64:
      url: gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc
      sha256Hash: a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63
    arm64:
      url: gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc
      sha256Hash: 1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9
  snapshotsConfig:
    location: gs://ate-snapshots/ate-demo-counter/
  workerPoolRef:
    name: counter
    namespace: ate-demo-counter
EOF
```

### Confirm Intercept

Create a fresh actor so it boots with the updated policy, then send an HTTPS
request through the actor:

```sh
kubectl ate create actor my-counter-mitm-example --template=ate-demo-counter/counter
kubectl ate resume actor my-counter-mitm-example --boot

curl -i \
  -H "Host: my-counter-mitm-example.actors.resources.substrate.ate.dev" \
  "http://localhost:8000/egress?url=https://example.com/"
```

The request should return `200`. The template above passes the CA certificate from
`example-mitm` to the counter actor as `EGRESS_CA_CERT_PEM`, and the demo app
adds that CA to its HTTPS client trust roots. Without that trust root, the
request fails with `x509: certificate signed by unknown authority`, which still
confirms interception is happening.
