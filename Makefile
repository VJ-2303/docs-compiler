.PHONY: help up down logs ps clean

help:
	@echo "Available commands:"
	@echo "  make up     - Start all infrastructure containers in background"
	@echo "  make down   - Stop and remove infrastructure containers"
	@echo "  make logs   - Tail logs from all containers"
	@echo "  make ps     - Check status and health of containers"
	@echo "  make clean  - Stop containers and remove all persistent volumes (DATA LOSS)"

up:
	docker compose -f deploy/docker/docker-compose.yml up -d

down:
	docker compose -f deploy/docker/docker-compose.yml down

logs:
	docker compose -f deploy/docker/docker-compose.yml logs -f

ps:
	docker compose -f deploy/docker/docker-compose.yml ps

clean:
	docker compose -f deploy/docker/docker-compose.yml down -v
