FROM golang:1.25.9 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/k8e-sandbox-cli ./cmd/sandboxcli

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/k8e-sandbox-cli /usr/local/bin/k8e-sandbox-cli
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/k8e-sandbox-cli", "mcp-serve"]
