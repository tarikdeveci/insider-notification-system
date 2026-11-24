.PHONY: test e2e lint fmt vet up down load
export CGO_ENABLED=1

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint:
	golangci-lint run ./...

up:
	docker compose up -d --build

down:
	docker compose down -v

# Exported to every command of the recipe: compose (worker env passthrough) and the tests, which
# recreate the worker for the priority scenario and must keep these values.
e2e: export RETRY_BASE_DELAY=200ms
e2e: export RETRY_MAX_DELAY=1s
e2e: export REAPER_INTERVAL=1s
e2e:
	docker compose up -d --build; rc=$$?; \
	if [ $$rc -eq 0 ]; then docker compose stop worker; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then E2E_PHASE=api go test -race -tags e2e -count=1 ./tests/e2e/...; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then docker compose start worker; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then E2E_PHASE=engine go test -race -tags e2e -count=1 ./tests/e2e/...; rc=$$?; fi; \
	docker compose down -v; exit $$rc

# Burst load test with k6 (grafana/k6 image). Owns ports 8080 and 9090 and always tears the stack down.
# On Git Bash for Windows export MSYS_NO_PATHCONV=1 so docker arguments are not path converted.
load:
	bash tests/load/run.sh
