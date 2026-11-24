.PHONY: test e2e lint fmt vet up up-tracing down
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

# Same stack plus Jaeger (UI on http://localhost:16686); api and worker export traces to it.
up-tracing: export OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318
up-tracing:
	docker compose --profile tracing up -d --build

down:
	docker compose --profile tracing down -v

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
