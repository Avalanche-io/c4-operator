# mlflow-c4

MLflow plugins for C4:

- **`mlflow run --backend c4`** runs an MLflow project as a C4 job;
- **`c4:///<view>[/path]`** artifact URIs let MLflow read artifacts and
  model versions that live in a C4 view.

Both reach the farm only through the C4 HUD API, as your own identity, so
the HUD checks and books everything they do. Apache-2.0, like the rest of
c4-operator.

## Install

```sh
pip install ./integrations/mlflow-c4      # from a c4-operator checkout
```

Needs Python 3.10 or later and MLflow 3.3 or later (tested with MLflow
3.17.1 on Python 3.13).

Configure the HUD in the environment:

| variable                                | meaning                                                         |
| --------------------------------------- | --------------------------------------------------------------- |
| `C4_HUD`                                | the HUD's URL, for example `https://c4.example.com`             |
| `C4_HUD_TOKEN` or `C4_HUD_TOKEN_FILE`   | your bearer token (the file is reread on every request)         |
| `C4_PROJECT`                            | the C4 project whose store holds the views                      |

## Run a project on the farm

```sh
mlflow run ./denoiser -e main -P alpha=0.5 --backend c4 \
  --backend-config '{"project": "ml", "params": {"steps": "20000"}, "deadline": "2026-12-01T18:00:00Z"}'
```

The backend:

1. stores the project's files in the C4 project's store as a view,
   uploading only what the store lacks;
2. creates the MLflow run in status `SCHEDULED`;
3. submits one WorkJob through the HUD: the entry point's command runs
   in the project's image (`docker_env` in MLproject, or `image` in the
   backend config) with the view mounted at `/project`, and
   `MLFLOW_RUN_ID` and the tracking URI in its environment;
4. tags the run `c4.intake=true`, `c4.job=<namespace>/<name>`,
   `c4.view`, and `c4.deadline` when you give one.

With c4-sync's MLflow Tracker running (`config/samples/tracker-mlflow`),
the run is adopted by its `c4.job` tag: c4-sync links it to that job
rather than submitting another, writes the job's work states to the run
(`SCHEDULED`, `RUNNING`, `FINISHED`, `FAILED` and `c4.*` tags), registers
each output as a model version with source `c4:///<view>`, and relays the
approver's production alias back to the job's wait.

C4 is deadline-driven: give `deadline` when the result matters by a time.
Without one the farm suggests a deadline it can meet.

Backend config keys: `hud`, `token_file`, `project` (required),
`namespace`, `image`, `class` (default `ml`), `params`, `deadline`,
`pools`, `mount` (default `/project`), `poll_seconds` (default 15). See
`src/mlflow_c4/backend.py`.

`cancel()` holds the job and stops its running units (the HUD's
`holdStop` nudge); the HUD has no delete, and a person can release the
hold.

## Read c4 artifacts

```python
import mlflow
mlflow.artifacts.download_artifacts("models:/denoiser/3", dst_path="model")
mlflow.artifacts.download_artifacts("c4:///c45Zx.../weights", dst_path="w")
```

The repository lists the view through `GET /views/{project}/{view}/files`
and fetches each file by its C4 ID, checking the bytes against the ID. It
is read-only: a view is immutable, and what a C4 job makes is published by
the farm.

## Does an MLflow server accept `c4://` model-version sources?

Measured on 2026-10-19 against MLflow 3.17.1 (`mlflow server`, SQLite
store, localhost), with and without this plugin installed in the server's
environment; `tests/test_server.py` repeats the measurement:

| source                      | server                       |
| --------------------------- | ---------------------------- |
| `c4://<view>`               | refused: the path is empty   |
| `c4://<view>/`              | refused: the path is empty   |
| `c4:///<view>`              | accepted                     |
| `c4:///<view>/model`        | accepted                     |
| `c4://<view>/model`         | accepted                     |
| `c4://<view>/model` + run   | accepted                     |

The server stores a source without resolving its scheme, so the plugin is
not needed on the server to register a version; the results were the same
with and without it. It refuses a source whose path is empty or relative.
Clients need the plugin to read a c4 version: `models:/` resolves to the
source, which only this repository can fetch. So c4-sync publishes whole
views as `c4:///<view>`.

## Tests

```sh
python -m venv .venv && .venv/bin/pip install -e '.[test]'
.venv/bin/python -m pytest
```

The tests use a fake HUD in-process, a local SQLite MLflow store, and one
real `mlflow server` on localhost (`MLFLOW_C4_NO_SERVER=1` skips it).
