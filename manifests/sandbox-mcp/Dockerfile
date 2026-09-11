FROM golang:1.25.9 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/k8e-sandbox-cli ./cmd/sandboxcli

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/k8e-sandbox-cli /usr/local/bin/k8e-sandbox-cli
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/k8e-sandbox-cli", "mcp-serve"]
