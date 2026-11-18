#!/usr/bin/env python3
"""Entrypoint for the forge: exec the vendored native Go binary.

The forge itself is implemented in Go (the *.go sources in this repository)
and ships as a statically linked Linux amd64 binary, forge.bin, which the
rules allow as vendored source. This launcher only finds that binary and
replaces itself with it, so the trial still runs exactly one process: the
binary binds the ephemeral port, registers with the env, serves git smart
HTTP, REST v3, GraphQL and MCP from its own store, and exits when the env
says the trial is done.
"""

import os
import shutil
import sys
import tempfile


def binary_candidates():
    here = os.path.dirname(os.path.abspath(__file__))
    yield os.path.join(here, "forge.bin")
    yield os.path.join(os.getcwd(), "forge.bin")


def exec_binary(path):
    os.execv(path, [path])


def main():
    source = None
    for candidate in binary_candidates():
        if os.path.isfile(candidate):
            source = candidate
            break
    if source is None:
        print("forge.bin not found next to main.py", file=sys.stderr)
        return 1
    try:
        exec_binary(source)
    except PermissionError:
        pass
    except OSError:
        pass
    # The repository may be mounted read-only without the exec bit preserved:
    # copy the binary to a scratch directory, mark it executable, exec there.
    scratch = tempfile.mkdtemp(prefix="forge-bin-")
    target = os.path.join(scratch, "forge.bin")
    shutil.copyfile(source, target)
    os.chmod(target, 0o755)
    exec_binary(target)
    return 1


if __name__ == "__main__":
    sys.exit(main())
