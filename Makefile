.PHONY: run build test docker-up docker-down

run:
	go run ./cmd/server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/call-worker ./cmd/server

test:
	go test ./...

docker-up:
	docker compose up --build

docker-down:
	docker compose down
