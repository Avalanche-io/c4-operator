# schema: the submission and tool formats

Two formats sit above the operator's API, so that nobody who submits work or
wraps a tool writes Kubernetes objects:

- **a submission** (`c4: submit/1`): what to do for one piece of production,
  in the studio's terms: project, shot, the tree to start from, the jobs
  (each a tool over frames), how they follow each other, checks, approvals,
  who answers questions, and when it is due. No priority, no images;
- **a tool** (`c4: tool/1`): how one kind of work runs: the command for one
  unit, its parameters (OpenJD parameter definitions, plus `show`, `from` and
  `affectsOutput`), what it reads and writes, what it needs, how its progress
  and errors are read, and a test.

| file                                | what it is                                                                                                                                                                                                       |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `submit-1.schema.json`              | JSON Schema 2020-12 for a submission, served as `GET /schema/submit`                                                                                                                                             |
| `tool-1.schema.json`                | JSON Schema 2020-12 for a tool, served as `GET /schema/tool`                                                                                                                                                     |
| `fixtures/farm.yaml`                | the farm the fixtures are checked against: projects, milestones, people, roles, tools                                                                                                                            |
| `fixtures/submit/pass/`             | every worked submission in the format's design, and every fragment wrapped whole                                                                                                                                 |
| `fixtures/submit/slice1.yaml`       | the passing submissions slice 1 cannot lower yet, each with its exact refusals; the rest it runs                                                                                                                 |
| `fixtures/submit/fail/`             | submissions that must be refused, each with `<case>.expect.json`                                                                                                                                                 |
| `fixtures/tool/pass/<tool>/`        | every worked tool, the snippets wrapped whole, and the tools the submissions name                                                                                                                                |
| `fixtures/tool/fail/`               | tools that must be refused, each with `<case>.expect.json`                                                                                                                                                       |
| `chain-1.schema.json`               | JSON Schema for the accountability chain (`chain/1`) every signed record and book entry carries; `chain.go` has its type, `ParseChain` and `Line`, the chain read aloud                                          |
| `fixtures/chain/`                   | chains that pass (each with its `.line.txt`, read with `names.yaml`) and chains refused, with their errors                                                                                                       |
| `lock-1.schema.json`                | JSON Schema for `tool.lock` (`c4: lock/1`): each image a tool names, pinned by digest                                                                                                                            |
| `card-1.schema.json`                | JSON Schema for a tree's card, `.c4/card.yaml` (`c4: card/1`): what the tree is, its source at a pinned commit or the job that made it, its licence; served as `GET /schema/card`                                |
| `fixtures/card/`                    | cards that pass (a hub import, a job's output, an upload) and cards refused, with their errors                                                                                                                   |
| `act-1.schema.json`                 | JSON Schema for an act statement (`act/v1`): one act, signed by its actor's own key, which the HUD's control record cites; served as `GET /schema/act`                                                           |
| `device-1.schema.json`              | JSON Schema for a device record (`device/v1`): one of a person's device keys as the farm enrolled it; served as `GET /schema/device`                                                                             |
| `fixtures/act/`, `fixtures/device/` | acts that pass (a person's, a confirmed one, an agent's, a unit's, a relayed one, a submission, a device removal) and device records (each with a real P-256 key and proof), and both refused, with their errors |
| `act.go`                            | `Act`, `Device`, `ParseAct`, `ParseDevice`, `ParamsID`, `DeviceID`, and `Canonical`, canonical JSON (RFC 8785)                                                                                                   |
| `types.go`                          | `Submission`, `Tool`, `Lock`, `Card` and their parts, as the lowering, the runner and the CLI read them                                                                                                          |
| `parse.go`                          | `ParseSubmission`, `ParseTool`, `ParseLock`, `ParseCard`, `ToJSON`: the shape checked, errors in the farm's words                                                                                                |
| `schema.go`                         | the embedded schemas and fixtures: `SubmitSchema()`, `ToolSchema()`, `LockSchema()`, `CardSchema()`, `Fixtures()`, `Cases()`                                                                                     |

## Two checkers, one set of answers

A schema checks a document's shape. The farm's check (`POST /submissions`,
`POST /tools`) adds what needs the farm: that an `after` names a job, that a
tool exists and takes the parameters given, that a format string names a
parameter, that a parameter with no default is given.

Each `expect.json` says which one refuses the case (`by: schema` or
`by: check`) and the exact errors the farm reports, path, message and fix:

```json
{"by": "check", "errors": [{"path": "jobs.movie.after",
  "message": "no job \"comp-v4\" in this submission",
  "fix": "jobs follow jobs by their key in the file; did you mean \"comp\"?"}]}
```

Paths are dotted, with list indexes in brackets (`params[1].type`).
`ParseSubmission` and `ParseTool` give every `by: schema` error in exactly
these words (every error, sorted by path), so the farm, the runner and the
CLI refuse a document alike. They read YAML or JSON, keep jobs in the order
written, keep dates as the text written, and refuse a key written twice.

`go test ./schema` checks that every passing fixture validates and parses,
that the parsers give each `by: schema` case's errors word for word and
agree with a full JSON Schema validator on every case, that every
`by: schema` case is refused for the reason it names (`schemaMentions`), that
every `by: check` case passes the schema (so it really needs the farm), and
that every tool a passing submission names is published in `farm.yaml`. The
farm's check is tested against the same fixtures in its own package.

## Personal keys: act/v1 and device/v1

Every act is signed by its actor's own key: a person's device key, an
agent's or a unit's own key, or, for a decision made in a tracker, the
tracker's relay, naming the person. The HUD carries the act out and signs
its own control record, which cites the act. The schemas check the shape;
this is what they can't say.

**The act** is an in-toto statement in a DSSE envelope, as every C4 record
is (`https://cccc.io/provenance/act/v1`).
- **Its subject** is exactly one: an object by kind, name and UID with the
  SHA-256 of its spec as the farm served it
  (`workjob:saltmarsh/sm0140-comp-v003@<uid>`), or content by its C4 ID (a
  submission, an output, a device). An act that makes something is on what it
  is made in, the project or the farm (`farm:<farm ID>`).
- **`farm`** is the C4 ID of the farm CA's root certificate (DER).
- **`params`** is the request's parameters as JSON, or the C4 ID of their
  canonical form when large. Two sets of parameters match when their
  `ParamsID`s do: the C4 ID of `Canonical` (RFC 8785), so key order, spacing
  and `1012.0` against `1012` never matter. A browser gets the same bytes from
  `JSON.stringify` of a key-sorted value. So that two different values never
  share a form, a number a double can't hold exactly as written (an integer
  past 2^53, more digits than a double keeps) and a lone surrogate escape are
  refused, as I-JSON (RFC 7493) rules them out: send such a number as a
  string.
- **`signedAt`** is when the signer signed, RFC 3339 in UTC, the time every
  C4 record carries. The farm refuses an act more than five minutes from its
  own clock. **`nonce`** is 16 random bytes, base64url.
- **`signer`** is the leaf certificate's identity. It follows `chain.by`: a
  person's act is signed by `user/<person>` and names its `device`; an agent's
  by `agent/<agent>`; a unit's by `unit/<project>/<job>/<unit>@<job UID>/<attempt>`;
  a tracker user's by the relay, `sa/<project>/<account>`, graded `relayed`.
  Only a person's act names a device. An act is never by a deployment's rule.
- **`chain`** is chain/1's `by`, `for`, `under` and `using`, as the actor
  states them. The deployments and whether the act is accountable are the
  control record's.

**Grades.** `signed`: the actor's own key signed the act. `confirmed`:
signed, and a person's touch key signed the same payload too; only a person
confirms. `relayed`: a tracker's relay signed it. `direct` and `claimed` stay
readable, because records made before personal keys keep them.

**Keys and signatures.**
- **A device's ID** is the C4 ID of its public key's DER
  SubjectPublicKeyInfo (`DeviceID`), the same in the leaf, the act and the
  device record.
- **The leaf** names its identity in a URI SAN, as every C4 leaf does, and a
  person's device or an agent's grant in a further one:
  `urn:c4:device:<device ID>`, `urn:c4:grant:<grant ID>`.
- **A device key is ECDSA P-256.** Its signature is raw `r||s`, 64 bytes,
  with a low S, so a signature has exactly one form. Farm parts stay Ed25519.
- **A confirmed act has two signatures over its one payload,** the device
  key's and then the touch key's, each with its key ID. The bundle carries
  the touch key's chain as `verificationMaterial.touchCertificateChain`, beside
  `x509CertificateChain`.
- **A browser's touch key is a passkey,** which signs only as WebAuthn. Its
  signature is an assertion: DER over `authenticatorData ||
  SHA-256(clientDataJSON)`, whose challenge is base64url of the SHA-256 of
  the DSSE payload's PAE. The bundle carries the assertion as
  `verificationMaterial.touchWebAuthn` (`authenticatorData`,
  `clientDataJSON`), beside the second signature. The verifier checks that:
  - the client data is a `webauthn.get` with that challenge, not from another
    origin's frame;
  - the person was present and verified (UP and UV);
  - the signature is the touch key's, its S folded to the low form first;
  - the relying party and origin are the ones the passkey's device record
    names.

  The CLI's touch key signs raw, as a device key does.
- **The farm takes each act once,** by the SHA-256 of the DSSE payload as
  signed, never by the envelope's C4 ID.

**The control record** gains `act`: the cited act's payload hash and its
record's C4 ID. It must match the act's subject, `act` and `params`, and no
act is cited twice.

**The device record** (`https://cccc.io/provenance/device/v1`) is signed by
the people's issuer. It holds the key, its protection (`hardware`,
`software-browser` or `software-file`), the kind of device and a label, and
the evidence the key was certified on:
- `id-token`: the studio's sign-in provider. The record keeps the ID token as
  issued, whose nonce is the device ID, and the C4 ID of the provider's key
  that signed it;
- `installer`: a one-person farm's owner, at the terminal, once;
- `owner`: a browser the owner's key enrolled, with the SHA-256 of its
  one-time code;
- `approval`: a confirmed act of the person's from another device.

A touch key is a device of its own, naming the device key it confirms for
(`touchFor`). A passkey's record has `webauthn` in place of `proof`: its
relying party (`rpId`), the HUD's `origin`, its `credentialId`, and its
enrolment assertion over the same request, whose challenge is base64url of
the SHA-256 of `ProofMessage`. A renewal is a record of its own, naming the record it renews.
`proof` is the key's own signature over its enrolment request, the canonical
JSON of `{farm, person, key, touchFor, nonce, requestedAt}`
(`ProofMessage`); `go test ./schema` verifies every fixture's.
