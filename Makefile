
.PHONY: migrate test run
DATABASE_PATH ?= data/app.sqlite3
migrate:
	mkdir -p $$(dirname "$(DATABASE_PATH)")
	@for f in migrations/*.sql; do \
		echo "applying $$f"; \
		sqlite3 "$(DATABASE_PATH)" < "$$f"; \
	done
test:
	go test ./...
run:
	go run ./cmd/server
