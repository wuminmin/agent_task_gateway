# TaskGate workflow-study kit

This directory is a **designed, not yet collected** workflow-study benchmark.
It must not be cited as expert or live-agent evidence until `protocol.json` is
updated through a reviewed evidence-promotion process and `analyze.py` accepts
the complete registered collection. The existing paper RQ5 therefore remains
a worked example rather than a workflow study.

## What is implemented

- 12 decision-oriented tasks across finance, support, and procurement;
- 445 expense claims, 553 support tickets, and 730 procurement payments;
- deterministic injected positive, negative, threshold-avoidance, entitlement,
  concentration, and conditional-drill-down cases;
- six catalog products with V3 release/influence/outcome accounting;
- database-generated ground truth in a schema unavailable to `gateway_reader`;
- differentiated 100-point rubrics with critical goals and trace guardrails;
- preregistered TaskGate, query-count, returned-row, and serialized-byte arms;
- disjoint nine-person calibration, nine-person approval, and six-person
  arm-blind grading panels, with three calibrators/approvers and two graders per domain;
- a tested buffer-before-release controller core for query, row, and byte baselines;
- a validator that rejects author budgets, incomplete seed coverage, missing
  common V3 risk, and placeholder collection records;
- a scorer that combines exact/numeric/list metrics with blind human scores.

Synthetic data makes the benchmark reproducible; it does not make the tasks
real-world validated. Domain practitioners must review task realism and supply
budgets independently before the study can support an external-validity claim.

## Task shapes

The task set contains four deliberately different workflows per domain:

| Domain | Task shapes |
|---|---|
| Finance | conditional monthly anomaly, employee concentration, policy join, aged pending liability |
| Support | SLA root cause, entitlement mismatch, seven-day repeats, legitimate zero-result confirmation |
| Procurement | split-payment evasion, vendor concentration, high-risk approval mismatch, conditional escalation |

Agents receive business requests, authorization envelopes, and a required JSON
answer shape. They do not receive SQL or hidden answers. Query traces determine
minimal-disclosure and stop-condition scores.

## Design validation

From the repository root:

```sh
python3 evaluation/workflow-study/validate.py
```

To validate the database, launch an isolated business service using the base
Compose file and `compose.yaml` in this directory. The SQL init sequence creates
the study data and hidden ground truth before granting the reporting views to
the gateway reader. Export truth only into the ignored `raw/` directory:

```sh
./evaluation/workflow-study/export-ground-truth.sh
```

The live reporting-view digest can be recomputed with the source-controlled
`evaluation/cmd/schema-digest` command. A mismatch is a schema drift failure,
not a value to update without review.

## Evidence collection

1. Recruit the registered disjoint non-author panels (three calibrators, three
   approvers, and two blind graders per domain) and complete the
   source-controlled unit-card training.
2. Have the calibration panel set at least three independent ceilings for every
   task/arm pair using `templates/expert-budget.example.json`; freeze medians.
3. Have the disjoint approval panel review those frozen requests using
   `templates/approval-review.example.json`; automatically retain decision time,
   rejection, and narrowing. Rejected tasks remain zero-completion observations.
4. Connect the supplied controller core to the Agent runner so baseline
   responses remain buffered before release. Query-count, row-count, and byte
   arms must still compute V3 facts in audit-only mode for the common risk axis.
5. Run every task/arm pair on seeds 0--4 with a fresh database and root task.
6. Strip arm and budget metadata, then collect trace and answer scores from
   blind graders using `templates/blind-grading.example.json`.
7. Validate and analyze only the complete registered collection:

```sh
python3 evaluation/workflow-study/validate.py \
  --truth raw/ground-truth.json --budgets raw/budgets \
  --approvals raw/approvals --runs raw/runs --gradings raw/gradings
python3 evaluation/workflow-study/analyze.py \
  --truth raw/ground-truth.json --budgets raw/budgets --approvals raw/approvals \
  --runs raw/runs --gradings raw/gradings --output raw/results.json
```

The actual LLM adapter and human records are intentionally not faked in this
design commit. Until they exist and the registered evidence is collected, this
directory is infrastructure, not a result.
