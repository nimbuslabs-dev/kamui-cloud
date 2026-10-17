# api

Coordinator for Kamui Cloud. Port 8080.

## Run

    cd api
    KAMUI_TOKEN=dev go run ./cmd/api

## Test

    cd api
    go vet ./...
    go test -race ./...
