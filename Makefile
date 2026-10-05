# Kwerft — common developer tasks. `make help` lists them.

GO            ?= go
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS       := -s -w -X github.com/ehilzinger/kwerft/internal/version.Version=$(VERSION) -X github.com/ehilzinger/kwerft/internal/version.Commit=$(COMMIT)
CONTROLLER_GEN = $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
IMAGE         ?= ghcr.io/ehilzinger/kwerft:$(VERSION)
# Kubernetes API server + etcd binaries for controller integration tests.
ENVTEST_K8S   ?= 1.37.0
SETUP_ENVTEST  = $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- build -------------------------------------------------------------

.PHONY: web
web: ## Build the console UI into web/dist
	cd web && npm ci && npm run build

.PHONY: dist-stub
dist-stub: ## Create a placeholder web/dist so Go builds without Node
	@test -f web/dist/index.html || { mkdir -p web/dist && echo '<!doctype html><title>Kwerft</title><p>UI not built — run <code>make web</code>.</p>' > web/dist/index.html; }

.PHONY: build
build: dist-stub ## Build bin/kwerft (run `make web` first for the real UI)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kwerft ./cmd/kwerft

.PHONY: image
image: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .

RELEASE_VERSION ?= 0.0.0-dryrun
.PHONY: release-dry-run
release-dry-run: web ## Build release artifacts into dist/release without publishing (RELEASE_VERSION=0.2.0)
	hack/release.sh dry-run $(RELEASE_VERSION) dist/release

## --- code generation ---------------------------------------------------

.PHONY: generate
generate: ## Regenerate deepcopy, apply configurations and CRDs from api/v1alpha1
	$(CONTROLLER_GEN) object paths=./api/...
	@# controller-gen leaves files of removed types behind; start from scratch.
	rm -rf api/applyconfiguration
	$(CONTROLLER_GEN) applyconfiguration paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=charts/kwerft/crds

.PHONY: verify-generate
verify-generate: generate ## Fail if generated files are out of date
	@git diff --exit-code -- api charts/kwerft/crds && test -z "$$(git status --porcelain -- api charts/kwerft/crds)" || { echo "Run 'make generate' and commit the result."; exit 1; }

## --- checks ------------------------------------------------------------

.PHONY: test
test: dist-stub ## Run Go tests, incl. controller tests against a real API server
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) --bin-dir $(CURDIR)/bin/envtest -p path)" $(GO) test ./...

.PHONY: lint
lint: dist-stub ## gofmt, go vet, web typecheck and unit tests
	@files=$$(gofmt -l . | grep -vE '^(web/node_modules|\.claude/worktrees|dist)/'); test -z "$$files" || { echo "$$files"; exit 1; }
	$(GO) vet ./...
	cd web && npm run typecheck && npm test

.PHONY: test-install
test-install: ## shellcheck + bats for the installer
	shellcheck install/install.sh install/join.sh hack/dev-server.sh hack/release.sh
	bats install/test

.PHONY: helm-lint
helm-lint: ## Lint and render the Helm chart
	helm lint charts/kwerft --set console.domain=ops.example.com --set acme.email=ops@example.com
	helm template kwerft charts/kwerft --set console.domain=ops.example.com >/dev/null
	@# Agent mode (install.sh --agent): no console Service, database or data key.
	helm template kwerft charts/kwerft --set mode=agent --set agent.consoleURL=https://ops.example.com \
		| grep -q -- '- --console-url=https://ops.example.com'
	! helm template kwerft charts/kwerft --set mode=agent --set agent.consoleURL=https://ops.example.com | grep -qE 'name: kwerft-data(-key)?$$'
	@# Backups: Velero's pre-backup hook copies the console's database (docs/phase6.md); agents have none.
	helm template kwerft charts/kwerft --set console.domain=ops.example.com \
		| grep -qF "pre.hook.backup.velero.io/command: '[\"/usr/local/bin/kwerft\", \"db-snapshot\", \"--data-dir=/var/lib/kwerft\"]'"
	helm template kwerft charts/kwerft --set console.domain=ops.example.com | grep -qx '        pre.hook.backup.velero.io/container: kwerft'
	helm template kwerft charts/kwerft --set console.domain=ops.example.com | grep -qx '        pre.hook.backup.velero.io/on-error: Fail'
	! helm template kwerft charts/kwerft --set mode=agent --set agent.consoleURL=https://ops.example.com | grep -q 'pre.hook.backup.velero.io'
	@# With the VictoriaMetrics operator's CRDs, as install.sh installs them (metrics-*.yaml).
	helm template kwerft charts/kwerft --set console.domain=ops.example.com --api-versions operator.victoriametrics.com/v1beta1 | grep -q 'kind: VMRule'

.PHONY: check
check: lint test test-install helm-lint ## Everything CI runs

## --- local development -------------------------------------------------

.PHONY: dev-api
dev-api: dist-stub ## Run the API on :8080 (pair with `make dev-web`)
	$(GO) run ./cmd/kwerft --listen=127.0.0.1:8080 --console-domain=localhost --platform=cloud --controllers=false --dev --data-dir=$(CURDIR)/bin/dev-data

.PHONY: dev-server
dev-server: ## Install/upgrade on a test server: make dev-server HOST=root@1.2.3.4 ARGS="--domain ops.example.com"
	@test -n "$(HOST)" || { echo "Set HOST, e.g. make dev-server HOST=root@203.0.113.24"; exit 1; }
	hack/dev-server.sh $(HOST) $(ARGS)

.PHONY: dev-web
dev-web: ## Run Vite on :5173, proxying /api to :8080
	cd web && npm run dev
