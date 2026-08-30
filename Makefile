# DB Scaling -- partitioning and sharding labs.
#

SHELL := /bin/bash
.DEFAULT_GOAL := help

ROWS    ?= 5000000
TENANTS ?= 500
SHARDS  ?= 4

# ------------------------------------------------------------------ meta
.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
	 | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: doctor
doctor: ## Check that docker / go / psql are usable
	@scripts/doctor.sh

# ------------------------------------------------------- single node (00-03)
.PHONY: up down reset psql
up: ## Start the single-node lab Postgres on :55432
	docker compose up -d --build pg
	@scripts/wait-for-pg.sh 55432

down: ## Stop everything (keeps volumes)
	docker compose --profile shards --profile reshard --profile citus --profile logical down

reset: ## Destroy every volume and start clean
	docker compose --profile shards --profile reshard --profile citus --profile logical down -v

psql: ## psql into the single-node lab database
	@scripts/psql.sh lab

# ------------------------------------------------------------------- labs
.PHONY: lab00 lab01 lab02 lab03
lab00: up ## Lab 00 -- baseline pain on one big table
	ROWS=$(ROWS) scripts/runlab.sh labs/00-baseline

lab01: up ## Lab 01 -- declarative RANGE partitioning + pruning
	ROWS=$(ROWS) scripts/runlab.sh labs/01-range-partitioning

lab02: up ## Lab 02 -- retention, DETACH/ATTACH, pg_partman
	ROWS=$(ROWS) scripts/runlab.sh labs/02-retention-rollover

lab03: up ## Lab 03 -- HASH + LIST multi-tenant partitioning
	ROWS=$(ROWS) TENANTS=$(TENANTS) scripts/runlab.sh labs/03-hash-list-multitenant

# --------------------------------------------------------- shards (04-05)
.PHONY: shards-up shards-init lab04 lab04-demo
shards-up: ## Start shards 0-3 on :55440-55443
	docker compose --profile shards up -d
	@for i in 0 1 2 3; do scripts/wait-for-pg.sh $$((55440+$$i)); done

shards-init: ## Apply the shard schema to every running shard
	SHARDS=$(SHARDS) scripts/shards-init.sh

lab04: build ## Lab 04 -- Go shard router: tests + guided demo
	go test ./... -count=1
	$(MAKE) shards-up shards-init
	./bin/shardctl seed   --tenants $(TENANTS)
	./bin/shardctl demo

lab04-demo: build ## Re-run just the lab 04 demo (no reseed)
	./bin/shardctl demo

.PHONY: reshard-up lab05
reshard-up: shards-up ## Add shards 4-7 for the 4 -> 8 reshard
	docker compose --profile reshard up -d
	@for i in 4 5 6 7; do scripts/wait-for-pg.sh $$((55440+$$i)); done
	SHARDS=8 scripts/shards-init.sh

lab05: build reshard-up ## Lab 05 -- online 4 -> 8 reshard with a cutover
	./bin/reshard plan
	./bin/reshard run

# ------------------------------------------------------------------ lab 06
.PHONY: citus-up lab06
citus-up: ## Start the Citus coordinator + 2 workers
	docker compose --profile citus up -d
	@scripts/wait-for-pg.sh 55450
	@scripts/citus-register.sh

lab06: citus-up ## Lab 06 -- Citus distributed tables and colocation
	@source scripts/env.sh; set -e; \
	for f in labs/06-citus/[0-9][0-9]-*.sql; do \
	  echo; echo "== $$f"; \
	  psql -h "$$PGHOST" -p "$$CITUS_PORT" -U "$$PGUSER" -d "$$PGDATABASE" \
	    -v ON_ERROR_STOP=1 -f $$f; \
	done

# ------------------------------------------------------------------ lab 07
.PHONY: logical-up lab07
logical-up: ## Start the publisher/subscriber pair
	docker compose --profile logical up -d
	@scripts/wait-for-pg.sh 55460
	@scripts/wait-for-pg.sh 55461

lab07: logical-up ## Lab 07 -- logical replication as a reshard primitive
	@scripts/lab07.sh

# ------------------------------------------------------------------- go
.PHONY: build test tidy fmt
build: ## Build shardctl + reshard into ./bin
	go build -o bin/shardctl ./cmd/shardctl
	go build -o bin/reshard  ./cmd/reshard

test: ## Run the Go unit tests (no database needed)
	go test ./... -count=1

tidy: ## go mod tidy
	go mod tidy

fmt: ## gofmt the tree
	gofmt -l -w .
