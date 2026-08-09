# Common development tasks for Mosscap.

default:
    @just --list

# Format all Go source files.
fmt:
    gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

# Run the test suite.
test:
    go test ./...

# Run Go's static checks.
vet:
    go vet ./...

# Run the tests and static checks.
check: test vet

# Build the CLI binary.
build:
    go build -o bin/mosscap ./cmd/mosscap

# Install the CLI binary to Go's configured binary directory.
install:
    go install ./cmd/mosscap

# Run Mosscap, forwarding any arguments (for example: just run -- --help).
run *args:
    go run ./cmd/mosscap {{args}}
