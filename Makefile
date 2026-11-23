.PHONY: test e2e lint fmt vet up down
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

e2e:
	RETRY_BASE_DELAY=200ms RETRY_MAX_DELAY=1s REAPER_INTERVAL=1s docker compose up -d --build; rc=$$?; \
	if [ $$rc -eq 0 ]; then docker compose stop worker; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then E2E_PHASE=api go test -race -tags e2e -count=1 ./tests/e2e/...; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then docker compose start worker; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then E2E_PHASE=engine go test -race -tags e2e -count=1 ./tests/e2e/...; rc=$$?; fi; \
	docker compose down -v; exit $$rc
