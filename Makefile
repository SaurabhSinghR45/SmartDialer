.PHONY: all build test sim loadtest up down clean run-dialer run-web

all: test build

up:
	@echo "Starting PostgreSQL, Backend, and Frontend via Docker Compose..."
	docker compose up -d postgres dialer web

down:
	@echo "Stopping Docker containers..."
	docker compose down -v

test:
	@echo "Running all backend unit and integration tests..."
	go test -v ./...

sim:
	@echo "Running full simulation suite (Scenarios A-D + Failure Cases 1-5 + Load Test)..."
	go run cmd/simulator/main.go --all

loadtest:
	@echo "Running 100 -> 1,000 agent concurrency load test..."
	go run cmd/simulator/main.go --load

build:
	@echo "Building Go backend binaries..."
	go build -o bin/dialer cmd/dialer/main.go
	go build -o bin/simulator cmd/simulator/main.go
	@echo "Building React frontend..."
	cd web && npm run build

run-dialer:
	go run cmd/dialer/main.go

run-web:
	cd web && npm run dev

clean:
	rm -rf bin/ simulation_results.json web/dist
