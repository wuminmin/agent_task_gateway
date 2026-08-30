# Agent-written workload lowerability

`questions.md` holds 40 natural-language reporting questions over the campaign
Products; `products.md` is the product sheet a text-to-SQL agent sees (the
`describe_data_product` view of `config/catalog.yaml`: fields, types, entity
keys, approved aggregates and operators). The SQL in `queries/` was written by
an off-the-shelf LLM assistant (Claude, via Claude Code) from those two files
alone -- it was not shown the closed fragment, the lowerer, or any repository
code, and its output was not edited. `results.json` records, per statement,
whether the production `internal/sqllowering` lowerer admits it and otherwise
the first rejection reported. Regenerate with `make eval-agent-workload-lowerability`.
