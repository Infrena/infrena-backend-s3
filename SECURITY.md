# Security policy

## Reporting a vulnerability

Report it privately through GitHub, using
[**Report a vulnerability**](https://github.com/infrena/infrena-backend-s3/security/advisories/new)
on this repository's Security tab. That opens a private advisory visible only to you and
the maintainers.

Please do not open a public issue for a security problem, and please do not disclose it
publicly until a fix is released.

Include the backend version, the engine version (`infrena version`), what you did and what
happened. Say which store you were using — S3 itself, MinIO, Backblaze B2 or another
S3-compatible service — since their behaviours differ.

If the problem is in the engine rather than this backend, report it against
[infrena/infrena](https://github.com/infrena/infrena/security/advisories/new).

## Supported versions

Only the most recent release. This backend is pre-1.0 and tracks the engine closely.

## What is in scope

This backend holds credentials to an object store and is the sole record of what your
infrastructure looks like. In particular:

- **Credential handling.** Anything that causes credentials to be logged, written to disk,
  or sent to an endpoint other than the one configured.
- **Locking.** Anything that lets two runs write state concurrently, or lets a run take a
  lock another run holds. Concurrent writes lose state.
- **Integrity.** Anything that can leave state truncated, partially written, or readable in
  a form the engine will misinterpret.
- **Isolation.** Anything that lets one environment read or write another's state.

## What is known, and not a vulnerability

- **State is stored in cleartext**, including sensitive attributes. That is the engine's
  documented position, not this backend's choice. Protect the bucket: restrict access, and
  turn on the store's own encryption at rest if you want it encrypted.
- **Anyone who can read the bucket can read your infrastructure**, including any secret
  recorded in it. The bucket policy is the security boundary.
