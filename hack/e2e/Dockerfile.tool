# The e2e tool image: c4e2e (test trees, mount checks, workloads) on Alpine.
# Built by hack/e2e/up.sh from a host-compiled binary.
FROM alpine:latest
ARG TARGETARCH
COPY bin/linux_${TARGETARCH}/c4e2e /usr/local/bin/c4e2e
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/c4e2e"]
