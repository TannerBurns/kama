FROM docker.io/library/golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG SOURCE_DATE_EPOCH=0

WORKDIR /src
COPY test/m1-functional/helper/main.go ./main.go
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/m1-functional-helper ./main.go

FROM scratch

ARG SOURCE_DATE_EPOCH=0

LABEL org.opencontainers.image.title="Kama M1 functional storage helper" \
      org.opencontainers.image.description="Non-production mmap, integrity, and ENOSPC fixture" \
      org.opencontainers.image.source="https://github.com/TannerBurns/kama" \
      org.opencontainers.image.licenses="Apache-2.0" \
      io.kama.image.nonproduction="true"

COPY --from=builder /out/m1-functional-helper /m1-functional-helper

USER 65532:65532
ENTRYPOINT ["/m1-functional-helper"]
