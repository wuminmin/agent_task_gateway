.PHONY: verify test build up down logs formal eval-validate eval-exposure eval-exposure-performance eval-exposure-scale eval-exposure-storage eval-provenance-baseline eval-daily-publication eval-daily-publication-online eval-daily-publication-validate eval-smoke eval-full artifacts fuzz paper-evidence paper paper-tkde paper-tdsc

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

eval-exposure:
	./evaluation/run-exposure.sh

eval-exposure-performance:
	./evaluation/run-exposure-performance.sh

eval-exposure-scale:
	./evaluation/run-exposure-scale.sh

eval-exposure-storage:
	./evaluation/run-exposure-storage.sh

eval-provenance-baseline:
	./evaluation/provenance-baseline/run.sh

eval-daily-publication-validate:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest evaluation/daily-publication/test_harness.py
	PYTHONDONTWRITEBYTECODE=1 python3 evaluation/daily-publication/evidence/validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest evaluation/daily-publication/evidence/test_validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 evaluation/daily-publication-online/evidence/validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest evaluation/daily-publication-online/evidence/test_validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 paper/tkde/rq5_evidence.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest paper.tkde.test_rq5_evidence
	bash -n evaluation/daily-publication/run.sh evaluation/daily-publication-online/run.sh
	docker compose --file evaluation/daily-publication-online/compose.yaml config --quiet
	docker run --rm -v "$(CURDIR):/src:ro" -w /src golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 go test ./evaluation/daily-publication/cmd/phase ./evaluation/cmd/rq5-online-transition ./cmd/snapshot-sidecar-install

eval-daily-publication:
	./evaluation/daily-publication/run.sh

eval-daily-publication-online:
	./evaluation/daily-publication-online/run.sh

eval-smoke:
	./evaluation/run.sh smoke

eval-full:
	./evaluation/run.sh full
	./evaluation/security/run-full.sh

artifacts:
	./evaluation/generate-artifacts.sh

fuzz:
	./evaluation/fuzz/campaign.sh

paper-evidence:
	python3 paper/tkde/generate_evidence.py

paper:
	./paper/tkde/build-container.sh

paper-tkde:
	./paper/tkde/build-container.sh

paper-tdsc:
	./paper/tdsc/build-container.sh
