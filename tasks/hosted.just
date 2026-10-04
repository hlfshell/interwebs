set working-directory := '../interwebs-hosted'

# Build the headless CLI without desktop dependencies.
build:
    GOWORK=off go build -o ../build/bin/interweb ./cmd/interweb

# Run the example, or pass a YAML config path relative to interwebs-hosted.
run config='examples/local.yaml':
    GOWORK=off go run ./cmd/interweb run --config '{{config}}'

# Validate a YAML configuration without starting the backend.
check config='examples/local.yaml':
    GOWORK=off go run ./cmd/interweb check --config '{{config}}'

test:
    GOWORK=off go test -race -count=1 -timeout 180s ./...
    GOWORK=off go vet ./...
