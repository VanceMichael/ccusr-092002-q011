
.PHONY: migrate test run
DATABASE_PATH ?= data/app.sqlite3
migrate:
	go run ./cmd/migrate
test:
	go test ./...
vet:
	go vet ./...
run:
	go run ./cmd/server
