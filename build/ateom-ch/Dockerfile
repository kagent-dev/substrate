# Build stage: compile ateom-ch and testinit from source.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /ateom-ch    ./cmd/ateom-ch
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /testinit   ./hack/testinit
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /goldeninit ./hack/goldeninit

# Runtime stage: ateom-ch + Cloud Hypervisor ecosystem binaries.
# Ubuntu 24.04 ships virtiofsd in its main repo; Debian bookworm does not.
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
        curl ca-certificates iproute2 \
    && rm -rf /var/lib/apt/lists/*

# virtiofsd — latest static musl binary from upstream (v1.13+, supports DEVICE_STATE).
RUN curl -fsSL \
    "https://gitlab.com/virtio-fs/virtiofsd/-/jobs/artifacts/main/raw/target/x86_64-unknown-linux-musl/release/virtiofsd?job=publish" \
    -o /usr/local/bin/virtiofsd \
    && chmod +x /usr/local/bin/virtiofsd

# cloud-hypervisor — static binary, no libc dependency.
ARG CH_VERSION=v52.0
RUN curl -fsSL \
    "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static" \
    -o /usr/local/bin/cloud-hypervisor \
    && chmod +x /usr/local/bin/cloud-hypervisor

# Guest kernel — the cloud-hypervisor team's own CI kernel; has CONFIG_VIRTIO_FS=y.
ARG KERNEL_TAG=ch-release-v6.16.9-20260508
RUN mkdir -p /usr/local/share/ateom-ch \
    && curl -fsSL \
       "https://github.com/cloud-hypervisor/linux/releases/download/${KERNEL_TAG}/vmlinux-x86_64" \
       -o /usr/local/share/ateom-ch/vmlinux

# grpcurl — used by the kind test script to drive ateom-ch over gRPC.
ARG GRPCURL_VERSION=1.9.1
RUN curl -fsSL \
    "https://github.com/fullstorydev/grpcurl/releases/download/v${GRPCURL_VERSION}/grpcurl_${GRPCURL_VERSION}_linux_x86_64.tar.gz" \
    | tar xz -C /usr/local/bin grpcurl

COPY --from=build /ateom-ch    /usr/local/bin/ateom-ch
COPY --from=build /testinit   /usr/local/share/ateom-ch/testinit
COPY --from=build /goldeninit /usr/local/share/ateom-ch/golden-init

ENTRYPOINT ["/usr/local/bin/ateom-ch"]
