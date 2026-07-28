# TaskGate expert-calibrated workflow-study kit

This directory implements the collection and analysis infrastructure for two
causally separate experiments. Its current status is **designed, not
collected**. The paper must continue to call the existing RQ5 evidence a worked
example until the independent human records and all registered Agent runs have
been collected and validated.

## What the two experiments test

Experiment A measures Agent utility at budgets frozen before any Agent run. It
uses 12 representative tasks (four each in finance, risk/compliance, and
customer operations), four primary policies, five replicates, a separate
unlimited diagnostic, and a registered budget-sensitivity sweep:

- 240 primary runs: 12 tasks × 4 policies × 5 replicates;
- 60 unlimited upper-bound runs: 12 × 1 × 5;
- 144 Pareto runs: 12 × 4 × 3 additional multipliers;
- 444 isolated runs in total.

The primary outcome is the prelocked 0–100 rubric score. Secondary outcomes
include task completion, numeric error, list precision/recall, unsupported and
factually wrong claims, runtime budget rejection, V3 exposure, query/row/byte
usage, latency, and storage.

Experiment B measures whether a disjoint expert panel can understand and act on
each budget representation. It records approval time, approve/reject/narrow,
budget edits, comprehension, and confidence. These decisions never gate,
relabel, or zero-score an Experiment-A run. That separation prevents approval
UX from being mistaken for Agent utility.

## Tasks and data

The deterministic snapshot contains 445 expense claims, 553 support tickets,
and 730 payments, plus policies, entitlements, and vendor attributes. Hidden
truth is created in a schema unavailable to the gateway reader. Each task has a
differentiated four-to-six-item 100-point rubric with at least one critical
goal; the tasks therefore do not mechanically produce the old 0/33/66/100
staircase.

| Domain | Representative workflows |
|---|---|
| Finance | conditional spend anomaly, employee concentration, policy-limit join, aged pending liability |
| Customer operations | SLA root cause, entitlement mismatch, seven-day repeat contact, legitimate negative-result confirmation |
| Risk/compliance | split-payment evasion, vendor concentration, high-risk approval mismatch, conditional escalation |

The cases are reproducible synthetic representatives, not historical tickets.
Before collection, at least one non-author domain practitioner must author or
substantively adapt each task and a different practitioner must independently
accept it using `templates/task-review.example.json`. The validator refuses to
promote the study without both contributions.

## Fair policy comparison

The primary arms are TaskGate V3, successful-query count, returned-row count,
and canonical serialized-byte count. The unlimited arm is only a quality upper
bound. Baseline responses are held in a private buffer and released only after
their controller admits them.

All arms are compared on a common offline V3 risk axis. The Control database
retains an immutable query-to-observed-fact relation, including facts that were
not novel at the root ledger. Analysis takes the union only over queries that
the policy actually released to the Agent. Thus a baseline result rejected by
the external row/byte/query controller cannot inflate its measured exposure or
pollute later deduplication. `sensitivity_weighted_exposure` sums the registered
1/3/5 sensitivity weight over distinct release, influence, and outcome facts;
record/field metrics come from their source FactIDs.

## Collection sequence

Run design checks first:

```sh
python3 evaluation/workflow-study/validate.py
```

For a collection snapshot, launch the study Compose stack and export hidden
truth only into the ignored raw directory with
`evaluation/workflow-study/export-ground-truth.sh`; the Agent-facing database
role cannot read that schema.

1. For every task, obtain one non-author practitioner who authors or
   substantively adapts the case and a different non-author practitioner who
   independently validates it. Store the signed provenance records in
   `evaluation/workflow-study/raw/task-reviews/`.
2. Recruit nine non-author calibration experts (three per domain). Experts see
   only the task, catalog, sensitivity labels, organization risk card, and unit
   cards. Collect three independent budgets per task/primary-arm cell in
   `evaluation/workflow-study/raw/budgets/`.
3. Lock the exact DeepSeek execution configuration. The version string should
   identify the provider release used on the collection date, not merely repeat
   the model alias:

   ```sh
   python3 evaluation/workflow-study/lock_execution.py \
     --model deepseek-chat --model-version PROVIDER-RELEASE \
     --output evaluation/workflow-study/raw/execution-lock.json
   ```

4. Freeze the component-wise lower medians, source-record hashes, task context,
   execution lock, relative MAD, and Kendall's W before any Agent run:

   ```sh
   python3 evaluation/workflow-study/freeze_budgets.py \
     --task-reviews evaluation/workflow-study/raw/task-reviews \
     --budgets evaluation/workflow-study/raw/budgets \
     --execution-lock evaluation/workflow-study/raw/execution-lock.json \
     --frozen-at 2026-08-01T00:00:00Z \
     --output evaluation/workflow-study/raw/budget-freeze.json
   ```

5. Create the immutable 444-cell schedule:

   ```sh
   ./evaluation/workflow-study/run-study.sh schedule \
     --freeze evaluation/workflow-study/raw/budget-freeze.json \
     --execution-lock evaluation/workflow-study/raw/execution-lock.json \
     --output evaluation/workflow-study/raw/schedule.json
   ```

6. Supply DeepSeek credentials only through the environment. Do not add them to
   the execution lock, schedule, shell scripts, or checked-in files:

   ```sh
   export DEEPSEEK_API_KEY='...'
   export WORKFLOW_EXECUTION_LOCK="$PWD/evaluation/workflow-study/raw/execution-lock.json"
   export WORKFLOW_AGENT_COMMAND='python3 evaluation/workflow-study/deepseek_agent_adapter.py'
   ./evaluation/workflow-study/run-study.sh run \
     --schedule evaluation/workflow-study/raw/schedule.json \
     --freeze evaluation/workflow-study/raw/budget-freeze.json \
     --execution-lock evaluation/workflow-study/raw/execution-lock.json \
     --output evaluation/workflow-study/raw/runs
   ```

   Every cell gets a fresh Compose project, Business and Control database,
   root task, cache namespace, and DeepSeek conversation. Completed cells are
   atomic and resumable. The isolated project and volumes are removed after the
   run; set `WORKFLOW_STUDY_KEEP_STACK=1` only for debugging one failed cell.

7. Independently collect nine budget-usability experts in
   `evaluation/workflow-study/raw/approvals/`.
   Collect two arm-blind grades per completed run from a third, six-person panel
   in `evaluation/workflow-study/raw/gradings/`.

8. Validate and analyze only a complete registered collection:

   ```sh
   python3 evaluation/workflow-study/validate.py \
     --truth evaluation/workflow-study/raw/ground-truth.json \
     --task-reviews evaluation/workflow-study/raw/task-reviews \
     --budgets evaluation/workflow-study/raw/budgets \
     --freeze evaluation/workflow-study/raw/budget-freeze.json \
     --execution-lock evaluation/workflow-study/raw/execution-lock.json \
     --approvals evaluation/workflow-study/raw/approvals \
     --runs evaluation/workflow-study/raw/runs \
     --gradings evaluation/workflow-study/raw/gradings

   python3 evaluation/workflow-study/analyze.py \
     --truth evaluation/workflow-study/raw/ground-truth.json \
     --task-reviews evaluation/workflow-study/raw/task-reviews \
     --budgets evaluation/workflow-study/raw/budgets \
     --freeze evaluation/workflow-study/raw/budget-freeze.json \
     --execution-lock evaluation/workflow-study/raw/execution-lock.json \
     --approvals evaluation/workflow-study/raw/approvals \
     --runs evaluation/workflow-study/raw/runs \
     --gradings evaluation/workflow-study/raw/gradings \
     --output evaluation/workflow-study/raw/results.json \
     --scored-csv evaluation/workflow-study/raw/scored-runs.csv \
     --decisions-csv evaluation/workflow-study/raw/expert-decisions.csv

   Rscript evaluation/workflow-study/mixed-models.R \
     evaluation/workflow-study/raw/scored-runs.csv \
     evaluation/workflow-study/raw/expert-decisions.csv
   ```

The Python report contains the preregistered paired TaskGate contrasts,
task-cluster bootstrap intervals, exact task-level sign-flip tests, Holm
correction over the three baselines, the unlimited diagnostic, four-point
utility–risk Pareto frontiers, calibration agreement, and the separate expert
usability outcomes. Queries are never treated as independent statistical
samples.
