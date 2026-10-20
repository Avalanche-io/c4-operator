# c4submit

The shared Python library under C4's DCC panels: it collects a scene's
files, reads its production context, checks a submission with the farm,
uploads only what the farm lacks, and submits. Standard library only,
Python 3.9 and up, so it runs inside Nuke, Houdini, Maya, Blender and
Katana as they ship. See `../README.md` for what a panel does and how it
is configured.

| module          | what it holds                                                                                   |
| --------------- | ----------------------------------------------------------------------------------------------- |
| `submitter.py`  | the flow: `Draft`, `prepare`, `Submitter.check`, `Submitter.submit`, `save`                     |
| `host.py`       | `Host`, `Renderable`, `Dependency`: what a DCC tells c4submit about its scene                   |
| `tool.py`       | a tool's definition and its parameters (Open Job Description's, with C4's `show` and `from`)    |
| `form.py`       | the submit form those parameters draw, without a toolkit: fields, groups, links, errors        |
| `submission.py` | the `submit/1` document: local checks with the farm's error shape, job keys                     |
| `collect.py`    | gathering files under the project folder, hashing them (with an ID cache)                       |
| `tree.py`       | a tree's ID, computed as the farm computes a view's (checked against `c4-operator tree-id`)     |
| `seq.py`        | sequence and tile paths: `####`, `%04d`, `$F4`, `<F4>`, `<UDIM>` and the rest                   |
| `frames.py`     | frame lists in Open Job Description's range grammar                                             |
| `farm.py`       | the HUD API client                                                                              |
| `summary.py`    | what a panel shows before Submit: the estimate and slack against the due time, in local time    |
| `yamlio.py`     | writing the submission as YAML without PyYAML                                                   |
| `testing.py`    | a fake farm (in memory and over HTTP) and a fake host, for panels' tests                        |

A panel calls `Draft` and `prepare` on its application's main thread (they
read the scene) and `check` and `submit` on a worker thread (they hash,
upload and wait on the farm), with a progress callback and a cancel test.
