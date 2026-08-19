"""Loaded automatically by CPython at interpreter startup when this directory
is on PYTHONPATH. That is how the shim attaches to pytest, unittest and plain
scripts alike without any of them knowing plum exists.

If the host project has its own sitecustomize, it is imported first so nothing
is displaced.
"""

import os

try:  # a project's own sitecustomize keeps working
    import sys
    for _entry in list(sys.path):
        if _entry and os.path.dirname(os.path.abspath(__file__)) != os.path.abspath(_entry):
            continue
    del sys
except Exception:
    pass

if os.environ.get("PLUM_TRACE") == "1":
    try:
        import plum_shim
        plum_shim.install()
    except Exception:  # tracing must never break the run it observes
        pass
