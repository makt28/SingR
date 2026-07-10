# ============================================================================
# SingR node image (SingR fork — NOT vanilla sing-box).
#
# This file was repurposed from upstream sing-box's Dockerfile to build the
# SingR SSPanel backend and ship a soga-style env/flag-driven entrypoint.
# Do NOT blindly overwrite it when resyncing sing-box upstream — see
# docker-entrypoint.sh / install-docker.sh / SingR-docker.sh, which form one
# unit with this image. Config lives in /etc/singr-docker (isolated from the
# bare-metal /etc/singr install so the two never collide on one host).
# ============================================================================
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
COPY . /go/src/github.com/sagernet/sing-box
WORKDIR /go/src/github.com/sagernet/sing-box
ARG TARGETOS TARGETARCH
ARG GOPROXY=""
ENV GOPROXY=${GOPROXY}
ENV CGO_ENABLED=0
ENV GOOS=$TARGETOS
ENV GOARCH=$TARGETARCH
RUN set -ex \
    && apk add --no-cache git build-base \
    && export COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
    && export CORE_VERSION=$(go run ./cmd/internal/read_tag 2>/dev/null || echo unknown) \
    && export SINGR_VERSION=$(cat release/singr/VERSION) \
    && export TAGS=$(cat release/DEFAULT_BUILD_TAGS_OTHERS) \
    && export LDFLAGS_SHARED=$(cat release/LDFLAGS) \
    && go build -v -trimpath -tags "$TAGS" \
        -o /go/bin/singr \
        -ldflags "-X 'github.com/sagernet/sing-box/constant.Version=$CORE_VERSION' -X 'github.com/sagernet/sing-box/poet/constant.Version=$SINGR_VERSION' $LDFLAGS_SHARED -s -w -buildid=" \
        ./cmd/sing-box

FROM --platform=$TARGETPLATFORM alpine AS dist
RUN set -ex \
    && apk add --no-cache --upgrade bash tzdata ca-certificates jq
COPY --from=builder /go/bin/singr /usr/local/bin/singr
COPY release/poet/server-docker.json /opt/singr/server.default.json
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
# Config, panel.json and certs are expected on a mounted volume at
# /etc/singr-docker (see install-docker.sh). The entrypoint seeds server.json /
# panel.json from env/flags on first start and refuses to start without a
# usable TLS certificate — same as the bare-metal binary.
VOLUME ["/etc/singr-docker"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
