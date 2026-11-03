"""One deterministic independently scored check per command invocation."""

import sys
from pathlib import Path

content = Path("report.txt").read_text()
if sys.argv[1] == "total":
    assert int(content.strip()) == 10
elif sys.argv[1] == "format":
    assert content == "10\n"
else:
    raise ValueError("Unknown unit test")
