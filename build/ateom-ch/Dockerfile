# Build stage: compile ateom-ch and testinit from source.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /ateom-ch      ./cmd/ateom-ch
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /testinit       ./hack/testinit

# Runtime stage: ateom-ch + Cloud Hypervisor ecosystem binaries.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        curl ca-certificates iproute2 \
    && rm -rf /var/lib/apt/lists/*

# cloud-hypervisor — static binary, no libc dependency.
ARG CH_VERSION=v43.0
RUN curl -fsSL \
    "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static" \
    -o /usr/local/bin/cloud-hypervisor \
    && chmod +x /usr/local/bin/cloud-hypervisor

# virtiofsd — serves the OCI rootfs directory into the VM as virtio-fs.
ARG VFD_VERSION=v1.11.0
RUN curl -fsSL \
    "https://github.com/virtio-fs/virtiofsd/releases/download/${VFD_VERSION}/virtiofsd" \
    -o /usr/local/bin/virtiofsd \
    && chmod +x /usr/local/bin/virtiofsd

# Guest kernel — the cloud-hypervisor team's own CI kernel; has CONFIG_VIRTIO_FS=y.
ARG KERNEL_TAG=ch-6.1.95-1
RUN mkdir -p /usr/local/share/ateom-ch \
    && curl -fsSL \
       "https://github.com/cloud-hypervisor/linux/releases/download/${KERNEL_TAG}/vmlinux" \
       -o /usr/local/share/ateom-ch/vmlinux

# grpcurl — used by the kind test script to drive ateom-ch over gRPC.
ARG GRPCURL_VERSION=1.9.1
RUN curl -fsSL \
    "https://github.com/fullstorydev/grpcurl/releases/download/v${GRPCURL_VERSION}/grpcurl_${GRPCURL_VERSION}_linux_x86_64.tar.gz" \
    | tar xz -C /usr/local/bin grpcurl

COPY --from=build /ateom-ch  /usr/local/bin/ateom-ch
# testinit is used to populate the OCI bundle rootfs inside the test pod.
COPY --from=build /testinit  /usr/local/share/ateom-ch/testinit

ENTRYPOINT ["/usr/local/bin/ateom-ch"]
