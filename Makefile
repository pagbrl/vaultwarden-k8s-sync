BINARY      := vaultwarden-k8s-sync
PKG         := ./cmd/vaultwarden-k8s-sync
IMAGE       ?= ghcr.io/osmose-club-hotels/vaultwarden-k8s-sync
TAG         ?= latest
BW_CLI_VERSION ?= 2024.9.0

GO          ?= go

.PHONY: all build test vet fmt tidy deps run docker docker-push clean

all: build

## Populate go.sum / download modules (requires network).
deps:
	$(GO) mod tidy

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/$(BINARY) $(PKG)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

## Run locally (needs bw on PATH + a kubeconfig; not the normal deploy path).
run: build
	./bin/$(BINARY)

docker:
	docker build \
		--build-arg BW_CLI_VERSION=$(BW_CLI_VERSION) \
		-t $(IMAGE):$(TAG) .

docker-push: docker
	docker push $(IMAGE):$(TAG)

clean:
	rm -rf bin
