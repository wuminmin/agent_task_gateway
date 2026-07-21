.PHONY: verify test build up down logs

verify:
	docker build --target verify -t taskbound-agent-data-gateway-verify .
	docker compose build
	./scripts/compose-test.sh

test:
	docker build --target verify -t taskbound-agent-data-gateway-verify .

build:
	docker compose build

up:
	docker compose up --build -d --wait

down:
	docker compose down

logs:
	docker compose logs -f gateway oa-demo
