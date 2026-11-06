FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/hfkv ./cmd/hfkv \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/hfkv-cli ./cmd/hfkv-cli

FROM alpine:3.20
RUN adduser -D -H -u 1000 hfkv && mkdir /data && chown hfkv /data
COPY --from=build /out/ /usr/local/bin/
USER hfkv
VOLUME /data
EXPOSE 6379
HEALTHCHECK --interval=30s CMD hfkv-cli -a "$HFKV_PASSWORD" ping || exit 1
ENTRYPOINT ["hfkv", "--bind", "0.0.0.0", "--dir", "/data"]
