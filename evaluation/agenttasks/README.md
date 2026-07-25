# Agent-task utility evaluation

This package separates planner correctness from task utility. It expands 24
database-analysis objectives across five matched release/influence budget and
history profiles, yielding 120 deterministic four-requirement traces with eight
candidate representations each.

Three policies see the same candidate utility evidence and budgets:

1. `taskgate_exact` uses exact release/influence sets and root history.
2. `utility_greedy` takes the highest individual-utility feasible candidate.
3. `taskgate_exact_no_history` ablates root history.

After selection, a scorer that does not read planner utility compares released
answer-token payloads with gold assertions. Task success requires at least two
of three gold assertions for every requirement. Budget feasibility is recomputed
from the selected exact effects. The campaign is run automatically by
`make eval-exposure` and embedded in `evaluation/exposure/results.json`.

This is a reproducible tool-policy trace replay. It measures the representation
planner's consequence under matched budgets; it is not a live-LLM or prompt
quality benchmark.
