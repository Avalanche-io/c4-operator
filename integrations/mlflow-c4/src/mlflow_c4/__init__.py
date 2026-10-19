"""MLflow plugins for C4.

- ``mlflow run --backend c4``: run an MLflow project as a C4 job
  (:mod:`mlflow_c4.backend`).
- ``c4:///<view>[/path]`` artifact URIs: read model versions and artifacts
  that live in a C4 view (:mod:`mlflow_c4.artifacts`).

Both talk to the farm only through the C4 HUD API, as the caller's own
identity, so the HUD checks and books everything they do.
"""

from mlflow_c4.c4id import identify

__all__ = ["identify"]
