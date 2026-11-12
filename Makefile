# SPDX-License-Identifier: GPL-3.0-or-later
# Operator flow:
#   make env                                   create .env, then edit it
#   make import PSU=/path/to/psu.iso [AOTI=/path/to/aoti.iso]
#   make up
# Developer targets use the pinned Go toolchain via scripts/go.

-include .env
PSU_EXTRACT_SRC ?= ./prettystellaruniverse/extract

COMPOSE := docker compose
GO := scripts/go

.PHONY: help env need-env golden test-data race import up down restart logs status psql ctl backup build test vet fmt check run test-db

help: ## Show the targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/' | expand -t 12

env: ## Create .env from the example if missing
	@test -f .env || { pw=$$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'); \
		sed "s/^POSTGRES_PASSWORD=$$/POSTGRES_PASSWORD=$$pw/" .env.example > .env; chmod 600 .env; \
		echo "created .env with a random POSTGRES_PASSWORD: now set PSU_GAME_ADDR"; }

need-env:
	@test -f .env || { echo "no .env: run make env and set POSTGRES_PASSWORD"; exit 2; }

data:
	mkdir -p $@

import: data ## Build ./data from your discs: PSU=/path/to/psu.iso [AOTI=/path/to/aoti.iso]
ifndef PSU
ifndef AOTI
	@echo "usage: make import PSU=/path/to/psu.iso [AOTI=/path/to/aoti.iso]"; exit 2
endif
endif
	docker build -t psu-extract "$(PSU_EXTRACT_SRC)"
	docker run --rm -v "$(abspath data):/data" -v psu-extract-cache:/work \
		$(if $(PSU),-v "$(abspath $(PSU)):/discs/psu.iso:ro") $(if $(AOTI),-v "$(abspath $(AOTI)):/discs/aoti.iso:ro") \
		psu-extract $(if $(PSU),--psu /discs/psu.iso) $(if $(AOTI),--aoti /discs/aoti.iso) --out /data

up: need-env ## Build and start psud and PostgreSQL in the background
	$(COMPOSE) up -d --build

down: ## Stop the stack (data stays in the volumes)
	$(COMPOSE) down

restart: ## Restart psud (after editing .env)
	$(COMPOSE) up -d psud

logs: ## Follow psud's log
	$(COMPOSE) logs -f psud

status: ## Show the containers and their health
	$(COMPOSE) ps

psql: ## Open a SQL shell on the database
	$(COMPOSE) exec postgres psql -U psu -d psu

ctl: ## Run a psuctl command in the running psud: make ctl ARGS="who" (see psuctl help-commands)
	docker compose exec psud psuctl $(ARGS)

backup: ## Dump the database to backups/
	mkdir -p backups
	$(COMPOSE) exec -T postgres pg_dump -U psu -d psu --format=custom > backups/psu-$$(date +%Y%m%d-%H%M%S).dump

build: ## Build the image from source
	$(COMPOSE) build psud

run: ## Run psud on this machine without Docker (accounts in memory, plain test ports on loopback)
	PSU_STATE_DIR=state PSU_PLAIN_PORTS=1 PSU_GAME_ADDR=$${PSU_GAME_ADDR:-127.0.0.1} $(GO) run ./cmd/psud

golden: ## Regenerate the rules golden vectors from prettystellaruniverse (needs ./data)
	mkdir -p work/golden
	python3 tools/golden.py prettystellaruniverse data work/golden/rules.json
	python3 tools/golden_combat.py prettystellaruniverse data work/golden/combat.json

test-data: ## Run the Go tests including those that need ./data and the golden vectors
	PSU_TEST_DATA=$(abspath data) PSU_TEST_GOLDEN=$(abspath work/golden) $(GO) test -count=1 ./...

race: ## Run the concurrent packages' tests under the race detector (needs gcc)
	PSU_TEST_DATA=$(abspath data) CGO_ENABLED=1 $(GO) test -race -count=1 ./internal/game/ ./internal/netx/ ./internal/auth/ ./internal/admin/

test: ## Run the Go tests
	$(GO) test ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format the Go sources
	$(GO) fmt ./...

check: vet test ## vet and test

test-db: ## Run the Go tests, including the store against a throwaway PostgreSQL container
	@id=$$(docker run -d --rm -e POSTGRES_PASSWORD=test -e POSTGRES_DB=psutest -p 127.0.0.1::5432 postgres:18); \
	trap "docker stop $$id >/dev/null" EXIT; \
	port=$$(docker port $$id 5432/tcp | head -1 | sed 's/.*://'); \
	until docker exec $$id pg_isready -U postgres -d psutest -h 127.0.0.1 >/dev/null 2>&1; do sleep 1; done; \
	PSU_TEST_DATABASE_URL="postgres://postgres:test@127.0.0.1:$$port/psutest?sslmode=disable" $(GO) test -count=1 $(TESTFLAGS) ./...
