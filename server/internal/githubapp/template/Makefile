.PHONY: build test lint smoke rollback preview release
build:
	@mkdir -p artifacts && printf 'build\n' > artifacts/build
test:
	@test -f .multica/environment.json
lint:
	@test -f AGENTS.md
smoke:
	@mkdir -p artifacts && printf 'smoke\n' > artifacts/smoke
rollback:
	@printf 'rollback hook (non-production)\n'
preview:
	@printf 'preview disabled by default\n'
release:
	@printf 'release disabled by default\n'
