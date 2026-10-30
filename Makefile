VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Set IMAGE to your registry path, e.g. REGION-docker.pkg.dev/PROJECT_ID/macfleet/macfleet-server
IMAGE   ?= REGION-docker.pkg.dev/PROJECT_ID/macfleet/macfleet-server
# Copy deploy/k8s/overlays/example to overlays/local (git-ignored), fill in real values, then make deploy.
OVERLAY ?= local
TAG     ?= $(VERSION)

.PHONY: build test agent-darwin image push deploy release

build:
	go build ./...

test:
	go vet ./... && go test ./...

# Agent binary for Apple Silicon Macs, written to dist/.
agent-darwin:
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/macfleet-agent-darwin-arm64 ./cmd/agent

image:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

push: image
	docker push $(IMAGE):$(TAG)

# See deploy/README.md for the one-time setup (secret, static IP, registry).
deploy:
	cd deploy/k8s/overlays/$(OVERLAY) && kustomize edit set image macfleet-server=$(IMAGE):$(TAG)
	kubectl apply -k deploy/k8s/overlays/$(OVERLAY)

# Manual release (use when the GitHub Actions workflow is not in the repo):
#   make release TAG=v0.1.0   builds both agent binaries, SHA256SUMS and install.sh, then publishes with gh.
release:
	@test -n "$(TAG)" || (echo "usage: make release TAG=vX.Y.Z" && exit 1)
	rm -rf dist && mkdir -p dist
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(TAG)" -o dist/macfleet-agent-darwin-arm64 ./cmd/agent
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(TAG)" -o dist/macfleet-agent-darwin-amd64 ./cmd/agent
	cp scripts/install.sh dist/install.sh
	cd dist && shasum -a 256 macfleet-agent-darwin-* install.sh > SHA256SUMS
	gh release create $(TAG) dist/* --title $(TAG) --generate-notes
