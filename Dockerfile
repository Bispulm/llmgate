# The gateway and the mock provider, in one small image that runs as a non-root user.
# Base images are pinned by digest, so a rebuild uses exactly the same inputs.
FROM golang:1.27.3-trixie@sha256:2336eda24930976591abea806d9b7d99eeae38238f19755c5deec02facdc1594 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llmgate ./cmd/llmgate && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llmgate-mock ./cmd/llmgate-mock

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/llmgate /out/llmgate-mock /usr/local/bin/
# Numeric, so that Kubernetes can verify runAsNonRoot. 65532 is distroless's nonroot user.
USER 65532:65532
EXPOSE 18300 18302 18303
ENTRYPOINT ["/usr/local/bin/llmgate"]
