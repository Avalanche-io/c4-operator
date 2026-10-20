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

| file                         | what it is                                                                                   |
| ---------------------------- | -------------------------------------------------------------------------------------------- |
| `submit-1.schema.json`       | JSON Schema 2020-12 for a submission, served as `GET /schema/submit`                         |
| `tool-1.schema.json`         | JSON Schema 2020-12 for a tool, served as `GET /schema/tool`                                 |
| `fixtures/farm.yaml`         | the farm the fixtures are checked against: projects, milestones, people, roles, tools        |
| `fixtures/submit/pass/`      | every worked submission in the format's design, and every fragment wrapped whole             |
| `fixtures/submit/fail/`      | submissions that must be refused, each with `<case>.expect.json`                             |
| `fixtures/tool/pass/<tool>/` | every worked tool, the snippets wrapped whole, and the tools the submissions name            |
| `fixtures/tool/fail/`        | tools that must be refused, each with `<case>.expect.json`                                   |
| `lock-1.schema.json`         | JSON Schema for `tool.lock` (`c4: lock/1`): each image a tool names, pinned by digest        |
| `types.go`                   | `Submission`, `Tool`, `Lock` and their parts, as the lowering, the runner and the CLI read them |
| `parse.go`                   | `ParseSubmission`, `ParseTool`, `ParseLock`, `ToJSON`: the shape checked, errors in the farm's words |
| `schema.go`                  | the embedded schemas and fixtures: `SubmitSchema()`, `ToolSchema()`, `LockSchema()`, `Fixtures()`, `Cases()` |

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
