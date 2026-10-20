# A stand-in block, for live runs without model spend

`claude` here is a fake Claude Code in print mode. It runs inside the real `c4-agent block-pod`, and it runs the real Stop hook from its `--settings`, as Claude Code does. So a live run exercises everything around the model:
- the hook's checkpoints, safe points, questions, answers and steering;
- `c4-unit`, the node daemon's relays and the queue;
- `block-pod`'s parts, holds, waits, needs and wakes.

It acts by the unit's role, its `C4_UNIT_NAME`:

| role      | what it does                                                                           |
| --------- | -------------------------------------------------------------------------------------- |
| `root`    | writes the job's expansion document (`$FAKE_EXPAND`) and is done                       |
| `plan`    | works in parts (`FAKE_PARTS`, `FAKE_PART_SECONDS`) and answers questions that reach it |
| `slow`    | the same, with long parts, so it answers late                                          |
| `port`    | asks `unit:plan`, and is done when the answer comes                                    |
| `slowask` | asks `unit:slow`, and is done when the answer comes                                    |
| `late`    | runs after `plan` and asks it, so `plan` is woken to answer                            |
| `docs`    | reports missing information (`.c4/needs.json`), and is done once it is supplied        |

`asks-plan.yaml` is a plan with all six roles. `c4-plan import -o expand.json asks-plan.yaml` turns it into the expansion document the root writes.

## Building

```sh
mkdir -p bin/fake-block
GOOS=linux GOARCH=$(go env GOARCH) CGO_ENABLED=0 go build -o bin/fake-block/c4-agent ./cmd/c4-agent
GOOS=linux GOARCH=$(go env GOARCH) CGO_ENABLED=0 go build -o bin/fake-block/c4-unit ./cmd/c4-unit
cp hack/e2e/fake-block/claude bin/fake-block/
docker build -f hack/e2e/fake-block/Dockerfile -t c4-block-fake:dev bin/fake-block
kind load docker-image c4-block-fake:dev --name c4-operator-e2e
```

## A job

A graph job:
- `root: root`;
- the plan's expansion document in `FAKE_EXPAND`, in the template's env;
- the image `c4-block-fake:dev`, `command: [c4-agent, block-pod]`;
- a writable input mounted inside `/private/tmp`;
- `CLAUDE_CONFIG_DIR` an emptyDir.

In the run of 2026-10-20, all four cases worked live:
- missing information, supplied through the HUD, then resumed;
- an answer within the hold;
- one past the hold that became a wait;
- a finished unit woken to answer.
