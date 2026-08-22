# P69 -05 pre-measurement failure

This directory is classified `DIAGNOSIS-NOT-FOR-PUBLICATION`. It is not a
formal campaign, publication evidence, a canary, or a v3 acceptance result.

The authorized diagnostic launcher built the formal Gateway from the clean,
pushed submission commit, then stopped during `phase1_bringup` before any
service became live. The exact Compose error is retained in
`deployments/concurrency-expense-detail/001/compose-up.log`:

```
failed to fetch metadata: fork/exec /usr/local/lib/docker/cli-plugins/docker-buildx: no such file or directory
```

The failing path was a stale symlink into the removed Docker Desktop WSL
distribution. The native `docker-buildx-plugin` and `docker-compose-plugin`
executables remained installed under `/usr/libexec/docker/cli-plugins`.
An attempted system-level symlink replacement did not run because `sudo`
required an interactive password. A shell-command sequencing mistake printed a
success-looking line after those `sudo` failures; that line is void and is not
evidence of a repair. The system symlink was not changed.

A task-local, mode-0700 Docker CLI configuration under `/tmp` was then bound
only to the installed native Buildx and Compose executables. A read-only
Compose build check completed with no warnings and left zero containers,
volumes, or networks. This changed neither the repository nor the system
plugin directory. It is the environment input for the separately authorized
single additional pre-measurement restart; this failed campaign directory will
not be reused.

The formal Gateway image was built, but live gates never ran and no observer,
operation, sample, migration record, Adapter stderr, or acceptance decision was
produced. Accordingly, this run neither reproduces nor disproves the callback
phase cliff and does not yield a v3 acceptance verdict. The launcher cleanup
left the exact Compose project with zero containers, zero volumes, and zero
networks. Rebuildable binaries remain only in the ignored local evidence tree;
the compact first-hand failure, build, configuration, and credential-audit
records are retained with the ledger entry.
