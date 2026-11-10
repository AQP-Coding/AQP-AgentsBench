"""Dummy evaluator: proves the evaluation pipeline works end to end.

It does NOT judge the agent's solution: it only confirms that the evaluator
container sees the final workspace (repository files and staged attachment).
Replace it with the real hidden checks before using this task for results.
"""

from pathlib import Path

for path in ("Dockerfile", "app/main.py", "reports/layers-report.txt"):
    assert Path(path).is_file(), f"{path} is missing from the evaluated workspace"
print("dummy check passed: workspace and attachment are visible to the evaluator")
