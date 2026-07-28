# Generated benchmark artifacts

This directory is intentionally empty in source control and excluded from the
Docker build context. It contains no human participant records, but formal run
records preserve admitted canonical responses and Fact-identity evidence so
offline scoring and exposure can be recomputed. Those payloads are synthetic
yet may contain values deliberately marked sensitive by the benchmark. Treat
the directory as sensitive research data: restrict access, use encrypted
storage where required, apply a documented retention period, and remove local
copies after producing a sanitized release artifact.

Do not commit this directory, API credentials, provider headers, hidden truth,
admitted response contents, Fact identities, or raw model traces. A publishable
artifact should export reviewed, sanitized records and aggregate scores to a
separate digest manifest while keeping the Agent-inaccessible hidden oracle out
of released prompts.

The required phase order is:

```text
execution lock
→ 18 unbudgeted held-out calibration workflows
→ algorithmic budget freeze
→ 636 evaluation workflows
→ policy-blind answer and trace-guard scoring
→ frontier analysis
```

Deleting this directory's generated contents discards local benchmark progress;
the runner's per-cell atomic files make an intact collection resumable. The
registered operating rule forbids deleting or selectively replacing calibration
cells before freezing. Local artifact hashes cannot prove compliance with that
rule after the fact; a stronger collection-integrity claim requires externally
anchoring each run digest and timestamp in an append-only service.
