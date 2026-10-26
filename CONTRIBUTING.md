# Contributing to c4-operator

Thank you for your interest. Please open an issue to discuss a large
change before the pull request.

## Getting started

You need Go 1.26, Docker, kind 0.33 and kubectl.

```sh
make tools        # controller-gen, setup-envtest and kind, at pinned versions
make test         # unit tests and envtest (a real API server and etcd)
make verify       # gofmt, go vet, generated code and CRDs in step, manifests render
make test-linux   # overlay, layer and capture tests in a privileged Linux container
```

`go run ./hack/sim` runs the scheduling simulation. The HUD runs against a
synthetic farm with no cluster: the mock that serves it lives in
[c4-hud](https://github.com/Avalanche-io/c4-hud).

## The end-to-end suite

The end-to-end phases run on an eight-worker kind cluster:

```sh
hack/e2e/up.sh      # kind (8 workers, 2 zones), MinIO, the operator, projects
hack/e2e/p1.sh      # views, capture, CSI
hack/e2e/p2.sh      # tiers and herds
hack/e2e/p3.sh      # isolation
hack/e2e/p5.sh      # the work queue (sets up pools; run before p4, p6, p7)
hack/e2e/p4.sh      # monorepo CI
hack/e2e/p7.sh      # provenance
hack/e2e/p6.sh      # the MCP evaluation
```

What they measured is in [docs/results.md](docs/results.md), and how the
parts fit in [docs/architecture.md](docs/architecture.md).

To work against a local checkout of the c4 module, use an uncommitted
`go.work` (`go work init . ../c4`) rather than a `replace` in `go.mod`.

## Pull requests

1. Branch from `main`.
2. Keep each change focused, with tests. Controllers and the queue are
   tested against envtest; code that mounts or captures has Linux tests
   behind `C4_PRIVILEGED_TESTS`.
3. Run `make verify` and `make test`. If you change the API in
   `api/v1alpha1`, run `make generate manifests` and commit the result.
4. Write commit messages as one line that says what changed and why.
5. Update DESIGN.md when a change alters the design, and the docs when it
   changes what a user does.

## Code

- Standard Go style, formatted with `gofmt`.
- Packages live under `internal/`; there is no `pkg/` directory.
- Prefer early returns to nested conditionals.
- Comments say why, not what.

## Writing

Documentation is plain and factual: say what the system does, what was
measured and how.

## Conduct and security

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).
Report security problems privately, as [SECURITY.md](SECURITY.md) describes.

## License

Contributions are made under the Apache License 2.0, the license of this
repository.
