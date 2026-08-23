# Offline server runbook

This bundle executes the TaskGate TKDE evaluation on an air-gapped Linux
server (no internet, ever). Everything needed is inside the bundle: docker
image tarballs, a git bundle of the exact measured commit, one generated
Compose `.env`, and these scripts.

## Server prerequisites

- x86_64 Linux, Docker Engine with the Compose v2 plugin, cgroup **v2**
  (`stat -fc %T /sys/fs/cgroup` must print `cgroup2fs`).
- ~40 GiB free disk under the offline root.
- Nothing listening on loopback ports 8082, 8092, 25433.
- If SELinux is Enforcing and containers cannot read bind mounts:
  `chcon -R -t container_file_t /opt/taskgate` (or set permissive mode).

## Layout

Upload the bundle to `/opt/taskgate/bundle` (override the root by exporting
`TASKGATE_OFFLINE_ROOT`). The scripts create:

```
/opt/taskgate/
├── bundle/                 # this bundle (images/, repo/, secrets/, server/)
├── agent_task_gateway/     # repo clone, made by 01-preflight.sh
└── results/                # logs + the final results tarball
```

## Run order

```
cd /opt/taskgate/bundle/server
./00-load-images.sh        # verify digests, docker load everything
./01-preflight.sh          # host checks, repo clone, .env, image retags
./02-validate.sh           # offline contract/profile validation
./03-smoke.sh              # synthetic harness smoke
./04-real-pilot.sh         # real-system pilot (fresh Compose deployment)
./06-formal.sh             # TLA+/TLC campaign (AFTER 04: dirties tree)
./99-collect-results.sh    # pack results for download
```

`07-optional-eval.sh` exists only if the bundle was built `--with-eval`.

Each script is idempotent and logs to `results/logs/`. Scripts 02-99 re-exec
themselves inside the `taskgate-toolbox:offline` container (Go toolchain,
make, jq, git, docker CLI + compose, warmed offline Go caches) with the
Docker socket mounted; you never need Go or git on the host. For debugging,
`./toolbox.sh` opens the same environment interactively.

Run 06 last: it rewrites tracked `formal/results/` files, and the real pilot
(04) refuses a dirty worktree.

Profile activation and the diagnosis/targeted campaigns are not part of this
runbook: they run through `evaluation/final-v5-wsl2/scripts/run-profile-campaign.sh`
(pilot class) with the private dataset binding, which materializes the
per-profile artifact directory itself (`TASKGATE_PROFILE_ARTIFACT_DIR`).

Profile activation and the diagnosis/targeted campaigns are not part of this
runbook: they run through `evaluation/final-v5-wsl2/scripts/run-profile-campaign.sh`
(pilot class) with the private dataset binding, which materializes the
per-profile artifact directory itself (`TASKGATE_PROFILE_ARTIFACT_DIR`).

## What must never happen here

- No `docker build` and no `go` downloads: everything runs `--no-build`
  against preloaded images with `GOPROXY=off`. If something tries to reach
  the network, that is a bug in the runbook — stop and report it.
- No publication campaign: every run here is pilot-class evidence with
  `publication_eligible=false` by construction. The three-deployment
  publication campaign stays on its frozen WSL2 contract.

## Results

`99-collect-results.sh` writes `results/taskgate-results-<ts>.tar.gz`
containing `evaluation/final-v5-wsl2/raw/` (pilot evidence),
`formal/results/`, all run logs, and an environment report. Download that one
file back; it contains no secrets.
