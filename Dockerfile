# syntax=docker/dockerfile:1
#
# NOT BUILT OR RUN IN THE ENVIRONMENT WHERE IT WAS WRITTEN — there is no
# Docker daemon there, so unlike the Go code in this repo, this file has not
# been executed even once. Treat it as a reviewed starting point, not a
# verified artifact: build it and run the smoke checks in
# docs/guides/04-deployment.md before relying on it.
#
# ---- build stage ------------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Copy manifests first so dependency download is cached independently of
# source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is not needed (chromedp speaks CDP over a websocket; goldmark is pure
# Go), so build a static binary that does not depend on the runtime image's
# libc version.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pdfsvc ./cmd/api

# ---- runtime stage ----------------------------------------------------------
FROM debian:bookworm-slim

# chromium         — the render engine itself
# fonts-*          — without fonts, text renders as blank boxes. This is the
#                    single most common "the PDF is empty" report for
#                    containerised HTML-to-PDF, because a slim base image
#                    ships no fonts at all. DejaVu covers Latin/Cyrillic/Greek;
#                    Liberation covers the Arial/Times/Courier metric
#                    substitutes that real-world CSS asks for by name; Noto
#                    CJK covers Chinese/Japanese/Korean.
# ca-certificates  — for fetching any https:// assets referenced by documents
RUN apt-get update && apt-get install -y --no-install-recommends \
        chromium \
        ca-certificates \
        fonts-dejavu-core \
        fonts-liberation \
        fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/*

# Run as a non-root user. Chromium's own sandbox is already disabled (it
# needs privileges containers do not grant), which means the container
# boundary is the only isolation layer around code that renders
# customer-supplied HTML — so not running that as root matters.
RUN useradd --system --create-home --uid 10001 pdfsvc
USER pdfsvc

COPY --from=build /out/pdfsvc /usr/local/bin/pdfsvc

ENV CHROMIUM_PATH=/usr/bin/chromium \
    PORT=8080

EXPOSE 8080

# No HEALTHCHECK here on purpose: this image has no curl/wget (deliberately,
# to keep the attack surface small), and orchestrators do their own HTTP
# probing against /livez and /readyz anyway. docker-compose.yml shows a
# working compose-level healthcheck.

ENTRYPOINT ["/usr/local/bin/pdfsvc"]
