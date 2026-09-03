# syntax=docker/dockerfile:1

##
## Stage 1: build the static Go binary
##
FROM golang:1.23-bookworm AS build

WORKDIR /src

# Cache modules first.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# CGO disabled -> a static binary that runs on the slim final image.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/vaultwarden-k8s-sync \
    ./cmd/vaultwarden-k8s-sync

##
## Stage 2: runtime image with the Bitwarden CLI (`bw`)
##
# We need `bw` at runtime because Vaultwarden speaks the Bitwarden
# password-manager API, which the CLI implements. Two supported install methods
# below — we use the npm package because it is version-pinnable and arch-agnostic.
#
# Alternative (commented): download the official release zip. That is a single
# self-contained binary (no Node needed) but is published for linux-x64 only, so
# it does not work on arm64 builders. Pick one:
#
#   ARG BW_VERSION=2024.9.0
#   RUN curl -fsSL -o /tmp/bw.zip \
#         "https://github.com/bitwarden/clients/releases/download/cli-v${BW_VERSION}/bw-linux-${BW_VERSION}.zip" \
#     && unzip /tmp/bw.zip -d /usr/local/bin \
#     && chmod +x /usr/local/bin/bw
#
FROM node:20-bookworm-slim AS runtime

# Pin the Bitwarden CLI version for reproducible builds.
ARG BW_CLI_VERSION=2024.9.0

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@bitwarden/cli@${BW_CLI_VERSION}" \
    && npm cache clean --force

# Run as an unprivileged user. `bw` stores its config/state under $HOME.
ENV HOME=/home/app \
    BITWARDENCLI_APPDATA_DIR=/home/app/.config/Bitwarden\ CLI
RUN useradd --create-home --uid 10001 app
USER 10001:10001

COPY --from=build /out/vaultwarden-k8s-sync /usr/local/bin/vaultwarden-k8s-sync

ENTRYPOINT ["/usr/local/bin/vaultwarden-k8s-sync"]
