# Credential leases

A `CredentialLease` says who may use which model credential, where and
until when, and keeps it renewed or ends it. To a model credential it is
what a cert-manager `Certificate` is to a TLS key.

This version carries the `holder-transfer` mode. A holder's own Claude Code
sign-in follows the holder's own work, from the holder's machine into the
unit pods that run it. The farm switch turns the mode on: it is the
manager's `--holder-transfer=allow`, and it is `disallow` by default.

## How it works

| part                                  | what it does                                                                                                                                                                             |
| ------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `c4-agent lease` (the holder's agent) | Runs on the holder's machine. It creates the lease and keeps its Secret holding the holder's current short-lived token, renewed before it expires. When it stops, it deletes the Secret. |
| the CredentialLease controller        | Keeps the lease's state. It deletes the Secret when the lease lapses, or when the farm's switch is off. It never reads the Secret: delete is its only verb on Secrets.                   |
| the work queue                        | Releases a unit whose template mounts a lease to a pool in the lease's target only while the lease is active and serves the job's owner. In other pools, the pod runs without it.        |
| `c4-lease-mounts` (admission policy)  | Only the queue creates pods that mount a lease's Secret. No one puts one in an environment variable or a projected volume.                                                               |

**The token, and only the token.**
- The agent reads the holder's sign-in where Claude Code keeps it. On macOS that is the login keychain item `Claude Code-credentials` (suffixed for a config dir other than `~/.claude`). Elsewhere it is `<config dir>/.credentials.json`.
- It writes the access token, its expiry and its scopes into the Secret, under the key `.credentials.json`.
- The refresh token never leaves the holder's machine. The holder's own Claude Code keeps refreshing the sign-in, and the agent copies each new token.
- Nothing prints, logs or returns the credential.

**The lease ends** at its TTL, at the token's expiry, when the agent stops, or when the farm turns the mode off. Whichever comes first, the Secret is deleted.

## Running it

On the holder's machine, with the holder's kubeconfig:

```sh
c4-agent lease --context my-farm --namespace practice --name joshua-claude \
  --holder joshua --owner joshua@studio --pool blocks \
  --ttl 8h --renew-before 15m --config-dir ~/.claude
```

It prints one line per renewal, saying when the lease is good until. Stop
it with Ctrl-C or SIGTERM, and the Secret is deleted.

| flag             | meaning                                                                                       |
| ---------------- | --------------------------------------------------------------------------------------------- |
| `--name`         | the lease, whose Secret is `c4-lease-<name>`                                                  |
| `--namespace`    | the namespace of the holder's jobs                                                            |
| `--holder`       | the person the sign-in belongs to                                                             |
| `--owner`        | an identity whose jobs are the holder's own: a WorkJob's `c4.avalanche.io/owner` (repeatable) |
| `--pool`         | a pool whose unit pods may mount it (repeatable)                                              |
| `--ttl`          | the lease's life, from its creation                                                           |
| `--renew-before` | how long before the token expires to renew it                                                 |
| `--config-dir`   | the Claude Code config dir whose sign-in it leases                                            |
| `--context`      | the farm's kubeconfig context                                                                 |

## A job that uses it

The template mounts the lease's Secret read-only. The pod's own Claude
Code store is an emptyDir, and `C4_LEASE_DIR` names the mount. Then
`c4-agent block-pod` links the store's credentials file to the lease's, so
a renewal reaches the running session.

```yaml
template:
  spec:
    containers:
    - name: block
      image: c4-block
      command: [c4-agent, block-pod]
      env:
      - {name: CLAUDE_CONFIG_DIR, value: /claude}
      - {name: C4_LEASE_DIR, value: /etc/c4/lease}
      volumeMounts:
      - {name: store, mountPath: /claude}
      - {name: lease, mountPath: /etc/c4/lease, readOnly: true}
    volumes:
    - {name: store, emptyDir: {}}
    - name: lease
      secret: {secretName: c4-lease-joshua-claude}
```

A unit of this job waits, without a pod, until the lease is active. The
queue logs why at verbosity 1: the lease is missing, not yet issued, ended,
not serving the job's owner, or the mode is off.

## The lease

| field                          | meaning                                                                                   |
| ------------------------------ | ----------------------------------------------------------------------------------------- |
| `spec.account`                 | the model account it serves                                                               |
| `spec.holder`                  | the person the credential belongs to                                                      |
| `spec.boundTo.owners`          | whose jobs it serves: only the holder's own                                               |
| `spec.mode`                    | `holder-transfer` (also `transfer` and `resident`, which this version does not carry yet) |
| `spec.target.pools`            | where it may be mounted                                                                   |
| `spec.ttl`, `spec.renewBefore` | its life, and when to renew                                                               |
| `status.state`                 | `active`, `renewing`, `waiting-for-holder`, `revoked` or `expired`                        |
| `status.expiresAt`             | when the credential or the lease ends, whichever is first                                 |
| `status.message`               | why, in plain words                                                                       |

The design is in proposals/agent-jobs 09, "Credential leases".
