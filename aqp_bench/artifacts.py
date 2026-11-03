"""Capture final changes without running Git or following agent-created links."""

import difflib
import hashlib
import os
import json
import stat
from pathlib import Path


def inventory(root):
    root = Path(root)
    files = {}
    for directory, dirs, names in os.walk(root, followlinks=False):
        dirs[:] = sorted(d for d in dirs if d not in (".git", "node_modules", ".venv", "__pycache__"))
        for name in sorted(names + [d for d in dirs if (Path(directory) / d).is_symlink()]):
            path = Path(directory) / name
            mode = path.lstat().st_mode
            relative = str(path.relative_to(root))
            if stat.S_ISLNK(mode):
                files[relative] = {"kind": "symlink", "target": os.readlink(path)}
            elif stat.S_ISREG(mode):
                digest = hashlib.sha256()
                with path.open("rb") as stream:
                    while chunk := stream.read(65536):
                        digest.update(chunk)
                files[relative] = {"kind": "file", "sha256": digest.hexdigest(),
                                   "executable": bool(mode & 0o111)}
    return files


def write_diff(before, workspace, destination, baseline=None):
    """Generate a Git-style patch without trusting agent-modified Git configuration."""
    after = inventory(workspace)
    lines = []
    for name in sorted(before.keys() | after.keys()):
        old, new = before.get(name), after.get(name)
        if old == new:
            continue
        a, b = json.dumps("a/" + name), json.dumps("b/" + name)
        lines.append(f"diff --git {a} {b}\n")
        def mode(item):
            return "120000" if item["kind"] == "symlink" else ("100755" if item["executable"] else "100644")
        if old is None:
            lines.append(f"new file mode {mode(new)}\n")
        elif new is None:
            lines.append(f"deleted file mode {mode(old)}\n")
        elif mode(old) != mode(new):
            lines.extend([f"old mode {mode(old)}\n", f"new mode {mode(new)}\n"])
        def text(root, item):
            if item is None:
                return ""
            if item["kind"] == "symlink":
                return item["target"]
            if root is None or (Path(root) / name).stat().st_size > 1024 * 1024:
                raise UnicodeError
            return (Path(root) / name).read_text()
        try:
            old_text, new_text = text(baseline, old), text(workspace, new)
            for line in difflib.unified_diff(old_text.splitlines(keepends=True), new_text.splitlines(keepends=True),
                                             fromfile=a if old else "/dev/null", tofile=b if new else "/dev/null"):
                lines.append(line if line.endswith("\n") else line + "\n\\ No newline at end of file\n")
        except UnicodeError:
            lines.append(f"Binary files {a} and {b} differ\n")
    Path(destination).write_text("".join(lines))
    return after
