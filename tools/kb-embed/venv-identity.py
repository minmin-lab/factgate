"""Print the identity of a kb-embed virtualenv as JSON.

The identity is the interpreter version, the sha256 of `pip freeze --all`, and
the sha256 over every installed distribution's RECORD file (pip writes one
hash per installed file there), in sorted path order. It is cheap to compute
and changes when any installed file pip knows about changes.

Usage: venv/bin/python tools/kb-embed/venv-identity.py
"""
import glob
import hashlib
import json
import os
import subprocess
import sys

prefix = sys.prefix
records = sorted(glob.glob(os.path.join(prefix, "lib", "python*", "site-packages", "*.dist-info", "RECORD")))
digest = hashlib.sha256()
for path in records:
    digest.update(os.path.relpath(path, prefix).encode() + b"\0")
    with open(path, "rb") as handle:
        digest.update(handle.read())
    digest.update(b"\0")
freeze = subprocess.run([sys.executable, "-m", "pip", "freeze", "--all"], check=True, capture_output=True).stdout
print(json.dumps({
    "version": "taskgate-kb-embed-venv-identity-v1",
    "python": sys.version.split()[0],
    "pip_freeze_sha256": hashlib.sha256(freeze).hexdigest(),
    "record_files": len(records),
    "records_sha256": digest.hexdigest(),
}, sort_keys=True))
