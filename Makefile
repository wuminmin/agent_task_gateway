.PHONY: verify test build up down logs formal eval-validate eval-smoke eval-full artifacts fuzz paper

verify:
	docker build --target verify -t taskbound-agent-data-gateway-verify .
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

formal:
	./formal/run.sh

eval-validate:
	./evaluation/run.sh validate

eval-smoke:
	./evaluation/run.sh smoke

eval-full:
	./evaluation/run.sh full
	./evaluation/security/run-full.sh

artifacts:
	./evaluation/generate-artifacts.sh

fuzz:
	./evaluation/fuzz/campaign.sh

paper:
	./paper/tdsc/build-container.sh
