# Build with the repository VERSION, for example:
# docker build --build-arg VERSION="$(cat VERSION)" .
FROM docker.io/library/golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG SOURCE_DATE_EPOCH=0

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build \
      -mod=readonly \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -buildid= -X github.com/TannerBurns/kama/internal/version.Version=${VERSION}" \
      -o /out/manager \
      ./cmd

FROM scratch

ARG VERSION=dev
ARG VCS_REF=unknown
ARG CREATED=1970-01-01T00:00:00Z
ARG SOURCE_DATE_EPOCH=0

LABEL org.opencontainers.image.title="Kama manager" \
      org.opencontainers.image.description="Kubernetes operator for AI model serving" \
      org.opencontainers.image.source="https://github.com/TannerBurns/kama" \
      org.opencontainers.image.url="https://github.com/TannerBurns/kama" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${CREATED}"

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/manager /manager

USER 65532:65532
ENTRYPOINT ["/manager"]
