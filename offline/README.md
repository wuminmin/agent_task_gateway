# Offline execution bundle

Tooling to run the TKDE evaluation on an air-gapped server when the
development host (a small NAS) cannot execute the harness itself.

Flow:

1. **NAS (online, this repo):** `sh offline/nas/build-bundle.sh [--with-eval]
   [OUT_DIR]` builds every image (application targets, TLC, the runner
   toolbox), pulls the pinned base images, saves them all as tarballs, writes
   a git bundle of HEAD, and generates the private Compose `.env`
   (`offline/nas/make-env.sh`). Verify with
   `sh offline/nas/selfcheck.sh OUT_DIR`.
2. **Transfer:** upload the bundle directory to the server at
   `/opt/taskgate/bundle` (file copy only; the server has no internet).
3. **Server (offline):** follow `offline/server/README.md` — numbered scripts
   load images, preflight, run validation/smoke/real-pilot/
   formal, then pack one results tarball.
4. **NAS:** download the results tarball and import it (raw evidence stays
   out of git per the existing policy; `formal/results/` is tracked and gets
   committed; status docs are updated from the evidence).

The toolbox image (`offline/toolbox.Dockerfile`) exists because the harness
runs `go run`/`make`/`jq`/`git` on the host while driving Docker Compose; on
the server those run inside the toolbox with the Docker socket mounted and
the repo bound at its host path, with `GOPROXY=off` guaranteeing no network
access. All server-side deployments are `up --no-build` against preloaded,
deterministically retagged images.

Pilot-class runs on the offline host declare
`TASKGATE_PILOT_HOST_CLASS=offline-linux` (see
`evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh`); publication mode does
not accept that class and remains WSL2-only.
