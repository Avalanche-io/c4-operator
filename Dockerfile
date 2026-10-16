# One image for the manager, the node agent, and the init container.
# Build with `make image`: the binary is compiled on the host first
# (see Makefile), then copied in.
FROM gcr.io/distroless/static-debian12:latest
ARG TARGETARCH
COPY bin/linux_${TARGETARCH}/c4-operator /c4-operator
ENTRYPOINT ["/c4-operator"]
