# P68b replacement pre-measurement failure

This directory is classified `DIAGNOSIS-NOT-FOR-PUBLICATION`. It is not a
formal campaign, publication evidence, a canary, or a v3 acceptance result.

The one authorised replacement deployment built the formal Gateway from the
clean, pushed `9418113249960bfae0efd91c4a761d77d2aab17d` tree and completed
profile activation. The repaired read-only cliff observer also emitted its
first snapshot successfully. The runner process was then invoked, but rejected
the 30-sample diagnostic configuration before constructing or sending any
operation:

```
pilot must declare a reviewed pilot kind, use one deployment, and use at most three samples per cell
```

The launcher had correctly generated the reviewed P68 shape: pilot class,
`real_system`, one deployment, one process replicate, five warmups and thirty
measured samples per each of the nine concurrency cells, with fresh roots and
the exact `DIAGNOSIS-NOT-FOR-PUBLICATION` marker. The defect was the remaining
generic `Config.Validate` pilot ceiling, which had no exact P68 exception.

The raw sample file and Adapter stderr file were never created. The later
credential scanner consequently reported `read input`; that is a downstream
symptom, not the failure origin. There are zero operations, migration records,
measured samples, or acceptance decisions. The sole observer snapshot is the
pre-operation zero-state baseline, so no migration curve, two-wait
decomposition, state correlation, cliff reproduction result, or `internal/`
culprit determination can be derived from this run.

The follow-up harness-only correction accepts the 30-sample shape only when all
reviewed P68 dimensions and the exact diagnostic marker match, with closed
negative controls for marker, experiment, pilot kind, density, replicate count,
and fresh-root behavior. It does not alter `internal/` or any measured system.
It was not live-retested because the task authorises exactly one replacement
deployment.

After the launcher exited, the exact Compose project was checked and had zero
containers, zero volumes, and zero networks. The ignored 4.6 GiB local evidence
tree remains on the workstation; only compact first-hand build, activation,
configuration, observer, failure, and credential-audit records are committed.
