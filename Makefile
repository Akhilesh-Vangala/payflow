.DEFAULT_GOAL := help
.PHONY: help build test test-short test-race test-idempotency lint fmt up down reset ps logs psql seed dashboard dashboard-build ml-data ml-train k6

FILTER := grep -v "🐳\|✅\|⏳\|🔔\|🚫"
B := backend
F := frontend/dashboard

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Compile every Go service
	cd $(B) && go build ./...

test: ## Unit + integration tests (requires Docker)
	cd $(B) && go test ./... -timeout 20m 2>&1 | $(FILTER)

test-short: ## Unit tests only, no containers
	cd $(B) && go test ./... -short

test-race: ## Integration tests under the race detector
	cd $(B) && go test ./... -race -timeout 30m 2>&1 | $(FILTER)

test-idempotency: ## Run the concurrency proof on its own
	cd $(B) && go test ./internal/payments/ -run TestConcurrentDuplicateIdempotencyKey -v -count=1 -timeout 10m 2>&1 | $(FILTER)

lint: ## go vet and staticcheck
	cd $(B) && go vet ./...
	@command -v staticcheck >/dev/null 2>&1 && (cd $(B) && staticcheck ./...) || \
		echo "staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest"

fmt: ## Format Go sources
	cd $(B) && gofmt -s -w .

up: ## Start the full local stack
	docker compose up -d --build

down: ## Stop the stack, keep data
	docker compose down

reset: ## Stop and destroy volumes (migrations re-run on next up)
	docker compose down -v

ps: ## Service health
	docker compose ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'

logs: ## Follow all logs
	docker compose logs -f

psql: ## Open a psql shell
	docker compose exec postgres psql -U payflow -d payflow

seed: ## Create a test merchant and print its API key
	cd $(B) && go run ./scripts/seed

dashboard: ## Run the merchant dashboard (needs `make up` and `make seed`)
	cd $(F) && npm install && npm run dev

dashboard-build: ## Type-check and build the dashboard
	cd $(F) && npm install && npm run build

ml-data: ## Download the Kaggle fraud datasets
	cd $(B) && python ml/download_data.py

ml-train: ## Train the fraud model
	cd $(B) && python ml/training/train.py --dataset sparkov

k6: ## Load-test the idempotency endpoint
	k6 run $(B)/test/load/idempotency.js
