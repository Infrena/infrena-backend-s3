# infrena-backend-s3

An [infrena](https://github.com/infrena/infrena) state backend that keeps a project's state in an
S3-compatible object store, and locks it with a conditional write.

Infrena starts this binary; you do not run it yourself. Install it, then name it in your project's
`backend:` block.

```yaml
backend:
  plugin: s3
  bucket: my-infrena-state
```

That is the whole minimum. Everything else has a default.

## Installing

```bash
infrena plugins install s3          # infrena 0.10.0 or newer
```

That is the easy path and the one to reach for. `infrena plugins install` learned about state
backends in **0.10.0**, and it resolves the kind from your project: a project whose `backend:` block
says `plugin: s3` gets this backend rather than a provider that happens to share the name. With no
name at all, `infrena plugins install` installs everything the project declares, the backend
included, which is what a fresh clone wants. Either way the version and checksum land in the
project's `plugins.lock`, and `infrena plugins verify` re-checks them.

Install writes to `<project>/.infrena/plugins/`, or to `~/.local/share/infrena/plugins/` with
`--global`.

**Placing the binary by hand still works, and still wins.** Install populates the plugin directories
infrena already searched rather than adding a second mechanism beside them,
so an `infrena-backend-s3` dropped into one of those directories is found exactly as an installed one
is — and one named by `--plugin-dir` or `INFRENA_PLUGIN_PATH` is found *first*, ahead of anything
installed. That is how you run a build you made yourself, and it is not a lesser option.

### Which infrena you need

Two different numbers, and the difference between them is real:

| Question | Answer |
| --- | --- |
| The oldest infrena that can **run** this backend | **0.8.0** — what `plugin.yaml`'s `infrena: ">= 0.8.0"` says |
| The oldest infrena that can **install** it | **0.10.0** |

0.8.0 is the release that introduced backends at all, and 0.8.0 and 0.9.0 run this binary exactly as
a newer host does. What they cannot do is fetch it: their `infrena plugins install` knows only
providers. On those two releases, download the archive for your platform from this repository's
releases and unpack `infrena-backend-s3` into one of the directories above — the backend then works
normally.

`plugin.yaml`'s floor is deliberately the running number rather than the installing one, because
running is the question the host asks when it loads the binary. A floor of 0.10.0 would refuse a
pairing that works.

## The `backend:` block

`plugin: s3` is infrena's key and picks this backend. **Every other key below is this plugin's**, and
an unknown one is an error naming it — this backend knows exactly which keys it reads, so a typo is
caught when the project loads rather than discovered later when state turns up somewhere unexpected.

| Key | Required | Default | What it is |
| --- | --- | --- | --- |
| `bucket` | **yes** | — | The bucket that holds this project's state. There is no sensible default for it. |
| `path` | no | the bucket root | A prefix inside the bucket. `/infrena/`, `infrena` and `infrena/` all mean the same place. |
| `region` | no | the store's own, or `us-east-1` when `endpoint` is set | The bucket's region. Most non-AWS stores either ignore it or expect `us-east-1`. |
| `endpoint` | no | `https://s3.amazonaws.com` | A store that is not AWS. **Must carry a scheme**: `http://` or `https://`. |
| `path_style` | no | `true` when `endpoint` is set, `false` otherwise | Address the bucket in the path (`host/bucket/key`) rather than in the hostname. Most self-hosted stores serve only path-style; AWS prefers virtual-host style. |
| `profile` | no | the default profile | Names a profile in your AWS credentials or config file. It **names** a credential; it never holds one. |

A fuller example, against a self-hosted MinIO:

```yaml
backend:
  plugin: s3
  bucket: infrena-state
  path: /projects/billing/
  endpoint: https://minio.internal:9000
  region: us-east-1
  path_style: true
  profile: infrena-state
```

### Keys and state

State for environment `production` is written to `<path>/production.json`, and its lock to
`<path>/production.lock` beside it. The backend builds every key itself, so the key space holds
exactly what this code put there: no key starts with a slash and none contains an empty segment.

`infrena state list` reports environments, not objects — the `.lock` files beside them are not
environments of their own.

## Credentials

**Credentials are never configuration.** `infrena.yml` is committed to git, so there is deliberately
nowhere in the `backend:` block to put a secret. Writing `access_key`, `secret_key`,
`access_key_id`, `secret_access_key`, `session_token` or `token` there is **refused**, pointing at
`profile:` instead. That refusal is the point: accepting one would have worked perfectly and put a
long-lived secret in a repository.

They resolve through a chain, first match wins:

1. **The named profile** in your shared AWS credentials or config file — `~/.aws/credentials` and
   `~/.aws/config`, or wherever `AWS_SHARED_CREDENTIALS_FILE` and `AWS_CONFIG_FILE` point.
   `profile:` selects which one.
2. **The environment** — `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and, for temporary
   credentials, `AWS_SESSION_TOKEN`.
3. **An instance role** — EC2, ECS or EKS credentials fetched from the instance metadata service.

This works unchanged for a store that is not AWS: the environment variables and the credentials file
are how an S3-compatible store's key and secret are supplied too.

## Locking, and why a store can be refused

**Every backend must lock, and there is no unsafe fallback.** A lock here is a conditional write:
the backend PUTs the lock object with `If-None-Match: *`, so the store itself decides which of two
simultaneous runs gets it, and there is no window between checking and taking. A second lock is
refused and told who holds the first.

Not every S3-compatible store implements conditional writes, so **support is proven rather than
assumed**. When infrena loads this backend, before it serves anything, it writes a throwaway object
twice with that header and requires the first write to succeed and the second to be refused with
`PreconditionFailed`. Then it deletes it.

That catches two different failures:

| What the store does | What the probe sees | Verdict |
| --- | --- | --- |
| Refuses the first write | an error immediately | **refused**, quoting what the store said |
| Accepts both writes | no error at all | **refused** — the header was ignored, so a lock here would never lock |
| Accepts the first, refuses the second | the expected `PreconditionFailed` | accepted |

The second row is the dangerous one. A store that ignores the header reports success, which is
exactly the shape of a lock that never locks: two applies could hold the same one and neither would
see an error anywhere. It costs one extra round trip at load time to rule out.

**A store that fails the proof is refused, not run unsafely**, and it is refused when the project
loads rather than when an apply is already under way.

## Store compatibility

Conditional-write support, established against the real services on 2026-09-17.

| Store | Conditional writes | Works with this backend | Tested by this repository |
| --- | --- | --- | --- |
| AWS S3 | yes | yes | **yes — full live suite** |
| MinIO | yes | yes | **yes — full live suite, plus end to end** |
| **Backblaze B2** | **no** | **no — refused at load** | **yes — the refusal** |
| Cloudflare R2 | yes | yes | no |
| DigitalOcean Spaces | yes | yes | no |
| Wasabi | unverified | unverified | no |

The last column is the one to read. The third column says what is believed; the fourth says what
this repository has actually run, and they are not the same thing.

**AWS S3 and MinIO** ran the whole live suite, which includes infrena's backend conformance suite
and ten goroutines racing for one lock with exactly one winner — a property a fake cannot
demonstrate, because its map is guarded by a mutex the real store does not have. MinIO additionally
ran an end-to-end pass through real infrena commands.

**Backblaze B2** ran the refusal: the backend rejected the bucket and quoted B2's own answer. A
passing B2 run would mean the probe is broken, not that B2 had improved.

**Cloudflare R2, DigitalOcean Spaces and Wasabi are untested here.** The first two are expected to
work and the third is unknown. If you use any of them, the backend tells you at load time whether
the store can lock, which is the whole point of proving rather than assuming — so an untested row
costs you a clear message rather than a corrupted lock.

### Backblaze B2 does not work, and here is why

B2 does not implement conditional writes. Its S3 API rejects `If-None-Match: *` outright:

```
PUT (If-None-Match: *) -> A header you provided implies functionality that is not implemented
                          error code: "NotImplemented"
```

So there is no way to take a lock on B2 that actually excludes a second run, and this backend
refuses the bucket rather than running without one. What you would see:

```
bucket "infrena" cannot be used for state: its store refused a conditional write, which is how this
backend locks. Every backend must lock and there is no unsafe fallback, so the bucket is refused
rather than run without one. The store's own answer: A header you provided implies functionality
that is not implemented (error code: "NotImplemented")
```

This is not a limitation that can be configured away, and it is better learned here than after
writing a configuration. If B2 is where your objects live, put infrena's state somewhere else — any
of the four stores above will do.

## Development

```bash
go build ./... && go test ./...          # unit tests, no store needed

docker compose up -d                     # MinIO on 127.0.0.1:9000
go test -tags live -count=1 ./internal/s3backend/
go test -tags e2e  -count=1 ./e2e/       # real infrena, real plugin, real bucket
```

The live and e2e suites **skip** when there is no store, saying how to start one.
`REQUIRE_LIVE_STORE=1` turns that skip into a failure, which is what CI sets: a suite that silently
skips reports green for tests that never ran.

The B2 refusal test is in the live suite and needs B2 credentials, which are read from the
environment and are never stored in this repository. Without them it skips, plainly, including under
`REQUIRE_LIVE_STORE` — CI has no B2 account and should not need one.

```bash
B2_KEY_ID=... B2_APP_KEY=... go test -tags live -run B2 ./internal/s3backend/ -v
```

`go.mod` requires a released infrena; a gitignored `go.work` pointing at a local checkout is how you
work against an unreleased one, and CI builds with `GOWORK=off` against the required release.

---

## Licence

Apache 2.0 — see [LICENSE](LICENSE).

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md), which explains the
[Contributor License Agreement](CLA.md) and why an open core project asks for one. Everyone
taking part is expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

Found a security problem? Please do not open an issue — [SECURITY.md](SECURITY.md) has the
private reporting path.
