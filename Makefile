
.PHONY: migrate test run
DATABASE_PATH ?= data/app.sqlite3
migrate:
	mkdir -p $$(dirname "$(DATABASE_PATH)")
	sqlite3 "$(DATABASE_PATH)" < migrations/001_bootstrap.sql
test:
	go test ./...
run:
	go run ./cmd/server
