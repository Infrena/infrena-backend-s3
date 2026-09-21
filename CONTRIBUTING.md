# Contributing to infrena-backend-s3

Thanks for wanting to help. This file is short, and most of it is about the one thing that
makes a state backend different from ordinary code: getting it wrong loses somebody's state.

## Before you start

**Open an issue first for anything non-trivial.** A pull request that changes how state is
written, locked or migrated is a conversation worth having before the code exists.

Small fixes — a typo, a wrong comment, a genuinely broken thing — just send them.

If the problem is in the engine rather than this backend, it belongs in
[infrena/infrena](https://github.com/infrena/infrena/issues). Planning, configuration and
the CLI all live there. This repository is only the S3 side.

## What this repository expects of code

**A backend is held to the conformance suite, not to its own opinion.** `pkg/backendtest`
in the engine defines what a backend must do, and this one passes it. If you change
behaviour, the suite is the thing to argue with first — if it disagrees with you, say so in
an issue rather than working around it here.

**Locking is not advisory.** A write that proceeds while another run holds the lock is the
failure this package exists to prevent. Anything touching the lock path needs a test that
would fail if the check were removed.

**A failure must not leave state half-written.** Uploads are all-or-nothing from the reader's
point of view. If you add a path that writes, say in its tests what happens when it dies
partway.

## Running the tests

```bash
# The ordinary suite. No credentials, no network.
go test ./...

# Against a real S3-compatible store. docker compose brings up MinIO.
docker compose up -d
go test -tags live -count=1 ./internal/s3backend/

# End to end, driving a real infrena binary against a real store.
go test -tags e2e -count=1 ./e2e/
```

The live and end-to-end suites **skip** when no store is reachable, and say why. CI sets
`REQUIRE_LIVE_STORE=1`, which turns that skip into a failure — a run that quietly skips the
only tests covering real object storage is a green tick that means nothing.

To point the live suite at something other than the local MinIO:

| Variable | Meaning |
| --- | --- |
| `INFRENA_LIVE_S3_ENDPOINT` | The store's URL |
| `INFRENA_LIVE_S3_ACCESS_KEY` | Access key |
| `INFRENA_LIVE_S3_SECRET_KEY` | Secret key |
| `INFRENA_LIVE_S3_REGION` | Region, where the store cares |

CI builds two ways: against the engine version `go.mod` requires (`GOWORK=off`), and against
the engine's current `main`. The first gates a merge; the second is early warning.

## Commit messages

Explain why, not what. The diff already says what changed.

## Why there is a CLA

Contributions are accepted under a [Contributor License Agreement](CLA.md). The reasoning is
the same as for the rest of Infrena, and it is set out in full there: the project is open
core, and the CLA is what lets contributed code ship in both halves without anyone's
contribution being relicensed out from under them.

Sign it once and it covers your contributions to every Infrena repository.

## Reporting problems

- **A security vulnerability:** [SECURITY.md](SECURITY.md). Do not open a public issue.
- **Anything else:** an issue here, or in the engine repository if that is where it lives.

By contributing you agree to abide by the [Code of Conduct](CODE_OF_CONDUCT.md).
