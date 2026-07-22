# Attack corpus

`corpus.json` is a versioned, non-performance corpus for AST-policy and full
TaskGate negative tests. `ALLOW_WITH_MANDATORY_SCOPE` means the query may be
accepted only after the trusted `eval_scope = 'all'` predicate is injected; it
is not an authorization bypass. Preserve the corpus version, exact Gateway
revision, stable returned code, and raw response for every published run.

`prompt-injection.json` maps untrusted text to representative SQL attempts for
the automated deterministic-boundary test. It makes no claim about model
robustness: the asserted boundary is only that physical/system objects and
non-SELECT statements are rejected by the Gateway policy. Agent credential
isolation is a separate deployment test and is not measured by this corpus.
