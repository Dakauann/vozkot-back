.PHONY: run seed test swagger db-up db-stop

run:
	go run ./cmd/server

seed:
	go run ./cmd/seed

test:
	go test ./...

swagger:
	swag init -g cmd/server/main.go --parseInternal --output docs

db-up:
	docker compose -f ../docker-compose.yml up -d database

db-stop:
	docker compose -f ../docker-compose.yml stop database
