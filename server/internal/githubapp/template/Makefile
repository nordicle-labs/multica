.PHONY: build test lint smoke rollback preview release
build:
	@mkdir -p artifacts && go build -trimpath -o artifacts/build .
test:
	@mkdir -p artifacts && go test ./... > artifacts/test
lint:
	@go vet ./...
smoke: build
	@mkdir -p artifacts && ./artifacts/build --smoke > artifacts/smoke
rollback:
	@rm -rf artifacts
preview:
	@printf 'preview disabled by default\n'
release:
	@printf 'release disabled by default\n'
