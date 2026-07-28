# TaskGate controlled Agent workflow benchmark

This directory defines a participant-free, controlled multi-domain benchmark
over business-motivated synthetic tasks. Its current status is **designed, not
collected**. DeepSeek is configured to execute real multi-turn query workflows
against isolated databases, while deterministic oracles score final answers and
registered trace guards separately. The benchmark does not recruit
experts, simulate experts with an LLM, or claim evidence from a real
organization.

The existing manuscript RQ5 remains a small policy-calibration worked example.
Only after every registered workflow has run and the frozen artifacts validate
may this directory support a separate controlled-Agent result.

## Registered design

The benchmark deliberately separates budget calibration from evaluation:

| Phase | Design | Workflows |
|---|---|---:|
| Held-out calibration | 6 tasks × unbudgeted × 3 replicates | 18 |
| Budgeted evaluation | 12 tasks × 4 policies × 4 levels × 3 replicates | 576 |
| Unbudgeted evaluation reference | 12 tasks × unbudgeted × 5 replicates | 60 |
| Evaluation total | budgeted plus unbudgeted | 636 |
| Overall total | calibration plus evaluation | 654 |

There are two calibration and four evaluation tasks in each of finance,
risk/compliance, and customer operations. Only their task identifiers,
objectives, and scoring roles are held out. Both phases deliberately operate on
the same frozen schema and exact synthetic snapshot. Calibration has a strict
answer envelope but no correctness oracle; its 18 outputs and usage traces
select budgets only and never enter an evaluation score or inference.

The four primary policies are:

- TaskGate V3 release, positive-output dependency, and outcome Fact ceilings;
- successful-query count;
- cumulative returned-row count;
- cumulative canonical serialized-byte count.

The arm whose machine identifier is `unlimited` is an unbudgeted
calibration/evaluation reference, not a
guaranteed quality upper bound or primary competitor. A stochastic Agent can
occasionally perform worse with more room to explore.

## Algorithmic budget freeze

No discretionary expert, simulated expert, or post-evaluation choice sets a
task-specific test budget. The authors predeclare the grid and derivation rule;
held-out Agent behavior supplies its inputs. A calibration run is eligible only
if it has completed status, exactly the registered answer fields, at least one
admitted query, no null field, and no empty string field. Zero, `false`, and
empty collections remain valid because they can represent legitimate aggregate
or negative answers. Correctness is not claimed or scored. For
each domain and native arm component, the calibration phase records six
unbudgeted observations—two tasks by three independent conversations—and takes
their component-wise lower median. With six sorted observations this is the
third value. Four evaluation ceilings are then computed as

```text
max(1, floor(level × domain_base))
```

for levels 0.25, 0.50, 0.75, and 1.00. TaskGate applies this rule separately to
release, influence, and outcome components. The other policies each have one
native component. Separate non-experimental operational sanity bounds prevent
pathological traces and resource runaway: TaskGate release/influence/outcome
facts are capped at 1,000,000/10,000,000/1,000, successful queries at 100,
cumulative returned rows at 100,000, and cumulative canonical response bytes at
100,000,000. These are not organizational risk preferences or study budget
levels. If a domain's component-wise calibration lower median exceeds its
corresponding bound, the campaign fails before any evaluation schedule exists.
The canonical freeze binds the calibration task and run digests, protocol,
database/oracle/risk/scorer sources, execution lock, domain bases, and every
derived ceiling. Evaluation requires both the freeze and the original 18
calibration records, so a copied freeze cannot hide source-run drift. Evaluation
outcomes can never change a ceiling.

This is a task-level calibration/evaluation split on one fixed synthetic
snapshot, not an independently sampled data split. It is not evidence that a
human could understand or approve the resulting units.

## Tasks, answer scoring, and trace guards

The database snapshot contains deterministic synthetic expense claims, support
tickets, payments, policies, entitlements, and vendor attributes. Evaluation
truth is stored in a schema unavailable to the Agent-facing database role.
These author-designed tasks cover different dependency patterns, including
aggregation and drill-down, joins, time windows, threshold and empty results,
sensitive small groups, and budget-constrained degraded answers. This coverage
does not establish empirical representativeness.

Each evaluation task has a prelocked four-to-six-component workflow rubric
totaling 100 points with at least one critical goal. The Agent must return
strict JSON with the registered conclusions, numeric values, record identifiers,
and rule fields. The policy-blind scorer reports three separate measures:

- `answer_score`: answer-valued rubric items normalized to 0–100; it is gated to
  zero unless admitted queries cover all task-registered evidence columns;
- `trace_guard_score`: query-attempt, stopping, allowed/forbidden-column, and
  forbidden-disclosure items normalized to 0–100;
- `workflow_rubric_score`: the original weighted combination, retained as a
  secondary workflow measure.

The deterministic checks include:

- categorical, rule, threshold, join, and time-window answers;
- numeric absolute, relative, tolerance-normalized error, and answer coverage;
- set/list-value precision, recall, F1, and unexpected elements;
- imperfect categorical, list, and numeric answer components;
- admitted-query coverage of task-registered evidence columns;
- task-specific query-attempt, allowed/forbidden-column, stopping, and
  forbidden-disclosure guards;
- schema validity and registered failures.

Narrative prose does not affect answer correctness, but it is inspected for
task-specific forbidden disclosure and can therefore affect the trace-guard and
combined workflow scores. The scorer never branches on policy identity. There
is no human or LLM blind-grading stage.

## Primary evidence and cross-policy checks

The primary analysis is evidence-eligible automatic `answer_score` versus the **actual**
sensitivity-weighted semantic exposure attributable to query responses admitted
to the Agent: the sum over distinct dimension-tagged release, influence, and
outcome entries. A FactID present in both release and influence contributes once
in each registered accounting dimension. Influence facts are supporting
dependencies, not necessarily values displayed to the Agent. Unknown
namespaces fail closed at high sensitivity; because derived Facts do not carry
field-level lineage, they take the highest configured field sensitivity among
their source relations. Independent stochastic replicates are first averaged
within each task–policy–level cell; equal replicate labels across policies do not imply
shared randomness. Analysis then compares task-level four-point
answer-score–exposure frontiers and reports domain-stratified task-bootstrap
uncertainty. Summary outcomes include:

- answer-score–exposure AUC on common observed support;
- exposure needed to reach 80% of mean unbudgeted-reference answer score;
- pairwise Pareto-dominance rate;
- critical-goal answer completion AUC, computed per task only on that task's
  four-policy common exposure support, with no-support tasks reported as
  unestimable.

Because TaskGate must not win merely by being evaluated only in its own unit,
the benchmark also reports checks computed uniformly for every policy:
`released_sensitive_records`, `released_sensitive_fields`,
`released_sensitive_cells`, `released_sensitive_values`,
`disclosed_outcome_propositions`, `disclosed_negative_propositions` (admitted
outcomes with zero rows), unnecessary sensitive fields, and native queries,
rows, and bytes. The record/field/cell/value/outcome views are derived from V3
Fact identities and are therefore not representation-neutral; query, row, and
byte counts are representation-independent volume counters. Curves are never
extrapolated beyond common observed support, and queries are diagnostics rather
than independent samples.

## Fair execution

Every evaluation cell is digest-bound to one execution lock and source set.
Together they preserve the campaign ID and lock timestamp, model provider and
alias, provider release label, temperature, `top_p`, `max_tokens`, request
timeout, maximum tool turns, system prompt, tool schema, data snapshot, answer
schema, and rejection envelope. A hosted alias can still drift behind the
provider boundary; the release label is recorded metadata, not a guarantee of
immutable model weights. Policy, level, and replicate execution order is
deterministically hash-randomized. Each workflow receives a fresh Compose
project, Business and Control database, root task, cache namespace, and model
conversation. A failure remains in the registered evaluation denominator only
when the source-locked adapter emits an auditable schema-v3 terminal record
with Fact evidence and a post-run budget audit. If the adapter process times
out, exits nonzero, emits invalid stdout, or emits an unverifiable record, the
runner cleans up and aborts collection; it does not invent a zero-exposure
observation.

For the three volume baselines, TaskGate V3 accounting runs audit-only in the
background. The Control database retains an immutable relation from each
settled query to every observed Fact, including non-novel facts. Analysis unions
facts only for responses actually released by the baseline controller, so a
buffered result rejected by a row, byte, or query ceiling cannot inflate its
measured exposure or contaminate later deduplication.

## Running the benchmark

First validate source-controlled design artifacts. With the study Compose stack
running, export hidden truth to the ignored raw directory. The Agent-facing
database role cannot read this schema:

```sh
python3 evaluation/workflow-study/validate.py
evaluation/workflow-study/export-ground-truth.sh
```

Lock the exact client-visible DeepSeek configuration. `--model-version` records
the provider release label used on the run date; it does not turn a hosted alias
into immutable weights. The lock tool records `locked_at` from current UTC;
calibration records must begin later. `--campaign-id` distinguishes a complete
collection, and the resulting lock SHA prevents records from different
campaigns being combined:

```sh
python3 evaluation/workflow-study/lock_execution.py \
  --model deepseek-chat \
  --model-version PROVIDER-RELEASE \
  --campaign-id workflow-2026-08-01-a \
  --temperature 0 \
  --top-p 1.0 \
  --max-tokens 4096 \
  --request-timeout-seconds 300 \
  --max-tool-turns 16 \
  --output evaluation/workflow-study/raw/execution-lock.json
```

Supply credentials only through the process environment. Never place a key in
the execution lock, schedule, shell scripts, logs, or checked-in files:

```sh
export DEEPSEEK_API_KEY='...'
export WORKFLOW_EXECUTION_LOCK="$PWD/evaluation/workflow-study/raw/execution-lock.json"
```

The runner invokes the source-locked DeepSeek adapter directly; it has no
arbitrary Agent-command option.

Create and execute the 18-cell held-out calibration schedule:

```sh
python3 evaluation/workflow-study/run_study.py calibration-schedule \
  --execution-lock evaluation/workflow-study/raw/execution-lock.json \
  --output evaluation/workflow-study/raw/calibration-schedule.json

python3 evaluation/workflow-study/run_study.py run \
  --schedule evaluation/workflow-study/raw/calibration-schedule.json \
  --execution-lock evaluation/workflow-study/raw/execution-lock.json \
  --output evaluation/workflow-study/raw/calibration-runs
```

Freeze the per-domain lower-median bases and derived four-level budget grid.
`--frozen-at` is an explicit RFC 3339 collection timestamp and must be later
than every calibration record's finish time:

```sh
python3 evaluation/workflow-study/freeze_budgets.py \
  --calibration-runs evaluation/workflow-study/raw/calibration-runs \
  --execution-lock evaluation/workflow-study/raw/execution-lock.json \
  --frozen-at "$(date -u +%Y-%m-%dT%H:%M:%S.%6NZ)" \
  --output evaluation/workflow-study/raw/algorithmic-budget-freeze.json
```

Only after that freeze validates, create and execute the 636-cell evaluation
schedule:

```sh
python3 evaluation/workflow-study/run_study.py evaluation-schedule \
  --freeze evaluation/workflow-study/raw/algorithmic-budget-freeze.json \
  --calibration-runs evaluation/workflow-study/raw/calibration-runs \
  --execution-lock evaluation/workflow-study/raw/execution-lock.json \
  --output evaluation/workflow-study/raw/evaluation-schedule.json

python3 evaluation/workflow-study/run_study.py run \
  --schedule evaluation/workflow-study/raw/evaluation-schedule.json \
  --freeze evaluation/workflow-study/raw/algorithmic-budget-freeze.json \
  --calibration-runs evaluation/workflow-study/raw/calibration-runs \
  --execution-lock evaluation/workflow-study/raw/execution-lock.json \
  --output evaluation/workflow-study/raw/evaluation-runs
```

The command-line tools reject phase inversion, digest drift, duplicate cells,
and incomplete calibration input.

The runner writes each completed cell atomically and is resumable. Isolated
projects and volumes are removed after a workflow. For single-cell debugging,
`WORKFLOW_STUDY_KEEP_STACK=1` retains the stack even when the adapter emits a
valid successful or terminal-failure record; never enable it for a formal
multi-cell campaign. External timeout, nonzero exit, invalid JSON, or an
unverifiable record always forces cleanup before the campaign aborts.

Formal run records retain admitted canonical response contents and Fact-identity
evidence for deterministic offline recomputation. Although the database is
synthetic, these records include benchmark-sensitive values. The entire `raw/`
directory is Git- and Docker-ignored and must be handled as sensitive research
data, access-restricted, retained only as long as required, and sanitized before
any artifact release.

Auditable evaluation failures remain terminal observations in the denominator.
An ineligible calibration cell instead aborts that calibration campaign. The
preregistered operating rule is to retain it and start a new prelocked campaign,
never delete or selectively replace cells to obtain a different budget base.
The lock, schedule, and freeze detect drift and mixing among retained artifacts,
but they are not an external append-only registry: artifact validation alone
cannot prove that an operator did not delete or replace a calibration output
before freezing. A collection that needs that stronger guarantee must anchor
run digests and timestamps in an independent append-only service as they are
created.

After all 654 workflows are present, validate before analysis:

```sh
python3 evaluation/workflow-study/validate.py \
  --truth evaluation/workflow-study/raw/ground-truth.json \
  --calibration-runs evaluation/workflow-study/raw/calibration-runs \
  --freeze evaluation/workflow-study/raw/algorithmic-budget-freeze.json \
  --execution-lock evaluation/workflow-study/raw/execution-lock.json \
  --runs evaluation/workflow-study/raw/evaluation-runs

python3 evaluation/workflow-study/analyze.py \
  --truth evaluation/workflow-study/raw/ground-truth.json \
  --calibration-runs evaluation/workflow-study/raw/calibration-runs \
  --freeze evaluation/workflow-study/raw/algorithmic-budget-freeze.json \
  --execution-lock evaluation/workflow-study/raw/execution-lock.json \
  --runs evaluation/workflow-study/raw/evaluation-runs \
  --output evaluation/workflow-study/raw/results.json \
  --scored-csv evaluation/workflow-study/raw/scored-runs.csv
```

The final artifact must retain the complete registered grid, including failed
and dominated observations, along with the execution lock, calibration freeze,
source digests, automatic score details, frontier summaries, and bootstrap
intervals.

## Claim boundary

If collected exactly as registered, the benchmark can support a statement such
as:

> On controlled business-motivated synthetic workflows, TaskGate changes the
> evidence-eligible automatic answer-score–exposure frontier relative to query-,
> row-, and byte-count accounting under the registered model alias and synthetic
> risk profile.

It cannot establish that experts understand Fact budgets, that approval is
faster or easier, that a real organization would choose these thresholds, or
that the synthetic cases are historical tickets. Those questions require a
separate human or production study and remain future work.
