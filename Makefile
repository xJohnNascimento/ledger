GOLANGCI_LINT_VERSION := v2.15.0
ACTIONLINT_VERSION := 1.7.12
SHELLCHECK_VERSION := v0.11.0
TERRAFORM_IMAGE := hashicorp/terraform:1.16.6
TFLINT_IMAGE := ghcr.io/terraform-linters/tflint:v0.64.0
GOMODCACHE := $(shell go env GOMODCACHE)

.PHONY: up down test lint proto proto-check image tf-validate

up: .env
	docker compose up --detach --build --wait

down:
	docker compose down --volumes

test:
	go test -race -count=1 ./...
	cd api && go vet ./...

lint:
	docker run --rm -v "$(CURDIR)":/src -v "$(GOMODCACHE)":/go/pkg/mod -w /src golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run
	docker run --rm -v "$(CURDIR)":/repo -w /repo rhysd/actionlint:$(ACTIONLINT_VERSION) -color
	docker run --rm -v "$(CURDIR)":/mnt -w /mnt koalaman/shellcheck:$(SHELLCHECK_VERSION) .github/scripts/*.sh

proto:
	go tool buf lint
	go tool buf generate

proto-check: proto
	git diff --exit-code -- api

image:
	docker build --tag ledger:local .

.env:
	@for client in CORRIDOR_WORKER CORRIDOR_AUDIT SEED OPERATOR; do \
		echo "LEDGER_SECRET_$$client=$$(openssl rand -hex 32)"; \
	done > $@
	@echo "generated local client secrets in .env"

tf-validate:
	docker run --rm -v "$(CURDIR)/infra":/infra -w /infra --entrypoint sh $(TERRAFORM_IMAGE) -c '\
		terraform fmt -check -recursive && \
		for env in envs/*/; do \
			terraform -chdir=$$env init -backend=false -input=false >/dev/null && terraform -chdir=$$env validate || exit 1; \
		done'
	docker run --rm -v "$(CURDIR)/infra":/infra -v tflint-plugins:/root/.tflint.d -w /infra -e GITHUB_TOKEN --entrypoint sh $(TFLINT_IMAGE) -c '\
		tflint --init --config=/infra/.tflint.hcl && tflint --recursive --config=/infra/.tflint.hcl'
