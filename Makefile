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
	docker compose up -d --build
	go test -race -tags e2e -count=1 ./tests/e2e/...; rc=$$?; docker compose down -v; exit $$rc
