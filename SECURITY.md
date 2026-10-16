# Security policy

## Supported versions

Security fixes go into the latest release.

| version | supported |
| ------- | --------- |
| v0.1.x  | yes       |
| < v0.1  | no        |

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it
privately through GitHub's private vulnerability reporting at
https://github.com/Avalanche-io/c4-operator/security/advisories ("Report a
vulnerability"), or by email to c4dev@avalanche.io.

Include what is affected (for example the node daemon, the cache tiers, the
CSI driver, a controller, the HUD, the MCP server or the certificate
issuer), how to reproduce it, and its impact. We aim to acknowledge a report
within 3 working days and to agree a disclosure date with you; reporters are
credited unless they prefer otherwise.

## What the operator trusts

[DESIGN.md](DESIGN.md) sections 1 and 6 give the trust model. In short:

- **The node daemon is privileged.** It runs with host networking as the CSI
  node plugin, mounts overlay filesystems and owns the node cache. Task pods
  need nothing beyond Pod Security `restricted`, and an admission policy
  lets node daemons change only output annotations of pods on their own
  node.
- **Store credentials** are Secrets in `c4-system`, read through a namespaced
  Role by the manager and the regional tier (and by node daemons only for
  projects in Direct mode).
- **Cache tiers authenticate every request** with projected service-account
  tokens (TokenReview) and authorize it by project, but in v0.1 they talk
  plain HTTP inside the cluster. Run them behind a mesh with mTLS in any
  shared cluster.
- **Content is verified where it enters** (ingest and capture hash every
  byte) and addressed by its C4 ID after that; the read path does not
  rehash.
- **Provenance records** are signed by each entity with a short-lived
  certificate from a farm signing CA whose key is a Secret in its own
  namespace, readable only by the manager. In production that key belongs in
  a KMS or HSM. A record's signing time is the signer's own claim until a
  timestamp authority is added.
- **The HUD's `--hud-dev-viewers` flag skips sign-in.** It is for local
  trials only; never set it on a shared cluster.
