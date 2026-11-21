# Release signing keys

These Ed25519 public keys verify a release's `SHA256SUMS.sig`. The release workflow signs with the private key in the
`RELEASE_SIGNING_KEY` secret of the `release` environment, and checks the signature against these files before it
publishes anything. `parcon` downloads nothing and checks no signature; users verify a release's files with them:

```bash
openssl pkeyutl -verify -pubin -inkey 2026-1.pem -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig
sha256sum -c --ignore-missing SHA256SUMS
```

See *Release signing* in [AGENTS.md](../../../AGENTS.md#releasing) for how to create, rotate, and
recover a key. Only public keys belong here; the private key never goes in the repository.
