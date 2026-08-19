"""PLUM's Python trace shim.

Shims are separate processes speaking the JSONL Event schema on a file or pipe.
The core orchestrates them; it never absorbs them. That is what keeps the tool
polyglot without the engine growing a Python dependency.

Usage (the collector sets these for you):

    PLUM_TRACE=1 PLUM_TRACE_OUT=/path/events.jsonl \
    PLUM_SYMBOLS=pkg/mod.py::Cache.get,pkg/mod.py::Cache.put \
    PYTHONPATH=shims/python python -X importtime -c "import plum_shim; plum_shim.install()" -m pytest

Only symbols named in PLUM_SYMBOLS are instrumented — the AST pass already
decided the instrumentation set, and paying for anything else is waste.
Requires CPython 3.12+ for sys.monitoring; falls back to sys.setprofile.
"""

from __future__ import annotations

import itertools
import json
import os
import sys
import threading
import time

SCHEMA_VERSION = "1.0"
_TOOL_ID = 4  # sys.monitoring tool id reserved for profilers

_out = None
_lock = threading.Lock()
_counter = itertools.count(1)
_local = threading.local()
_symbols: set[str] = set()
_max_events = int(os.environ.get("PLUM_TRACE_MAX", "200000"))
_written = 0


def _emit(**fields) -> None:
    global _written
    if _out is None or _written >= _max_events:
        return
    event = {
        "schema_version": SCHEMA_VERSION,
        "ts_ns": time.time_ns(),
        "test_id": os.environ.get("PLUM_TEST_ID", ""),
    }
    event.update(fields)
    with _lock:
        _out.write(json.dumps(event) + "\n")
        _out.flush()
        _written += 1


def _stack() -> list:
    if not hasattr(_local, "stack"):
        _local.stack = []
    return _local.stack


def _truncate(value: object, limit: int = 200) -> str:
    try:
        text = repr(value)
    except Exception:  # a __repr__ that raises must not break the run
        text = "<unrepresentable>"
    return text if len(text) <= limit else text[:limit] + "..."


def _symbol_id(code) -> str:
    path = os.path.relpath(code.co_filename, os.environ.get("PLUM_REPO_ROOT", os.getcwd()))
    qual = getattr(code, "co_qualname", code.co_name)
    return f"{path}::{qual}"


def _args_of(code) -> dict:
    """The arguments the function was actually called with.

    sys.monitoring hands the callback a code object, not a frame, so the frame
    is fetched from the stack. Best effort by design: a trace with no arguments
    is still evidence, a crashed test run is not.
    """
    try:
        frame = sys._getframe(2)
        if frame.f_code is not code:
            return {}
        names = code.co_varnames[:code.co_argcount + code.co_kwonlyargcount]
        return {n: _truncate(frame.f_locals[n]) for n in names if n in frame.f_locals and n != "self"}
    except Exception:
        return {}


def _on_call(code, _offset):
    symbol = _symbol_id(code)
    if _symbols and symbol not in _symbols:
        return
    stack = _stack()
    invocation = f"{threading.get_ident()}-{next(_counter)}"
    parent = stack[-1][1] if stack else ""
    _emit(event="call", symbol_id=symbol, invocation_id=invocation,
          parent_invocation_id=parent, depth=len(stack), args=_args_of(code))
    stack.append((symbol, invocation, len(stack)))


def _on_return(code, _offset, retval):
    stack = _stack()
    if not stack:
        return
    symbol = _symbol_id(code)
    if _symbols and symbol not in _symbols:
        return
    if stack[-1][0] != symbol:
        return
    _, invocation, depth = stack.pop()
    _emit(event="return", symbol_id=symbol, invocation_id=invocation,
          depth=depth, result=_truncate(retval))


def _on_raise(code, _offset, exc):
    stack = _stack()
    if not stack:
        return
    symbol = _symbol_id(code)
    if _symbols and symbol not in _symbols:
        return
    if stack[-1][0] != symbol:
        return
    _, invocation, depth = stack.pop()
    _emit(event="raise", symbol_id=symbol, invocation_id=invocation,
          depth=depth, exception=_truncate(exc))


def install() -> None:
    """Attach the shim. Safe to call twice; a no-op without PLUM_TRACE_OUT."""
    global _out, _symbols
    path = os.environ.get("PLUM_TRACE_OUT")
    if not path:
        return
    _out = open(path, "a", buffering=1)
    _symbols = {s for s in os.environ.get("PLUM_SYMBOLS", "").split(",") if s}

    monitoring = getattr(sys, "monitoring", None)
    if monitoring is None:  # pragma: no cover - CPython < 3.12
        _install_legacy()
        return
    monitoring.use_tool_id(_TOOL_ID, "plum")
    events = monitoring.events
    monitoring.set_events(_TOOL_ID, events.PY_START | events.PY_RETURN | events.RAISE)
    monitoring.register_callback(_TOOL_ID, events.PY_START, _on_call)
    monitoring.register_callback(_TOOL_ID, events.PY_RETURN, _on_return)
    monitoring.register_callback(_TOOL_ID, events.RAISE, _on_raise)


def _install_legacy() -> None:  # pragma: no cover - CPython < 3.12
    def profile(frame, event, arg):
        if event == "call":
            _on_call(frame.f_code, 0)
        elif event == "return":
            _on_return(frame.f_code, 0, arg)
        return None

    sys.setprofile(profile)
    threading.setprofile(profile)


if os.environ.get("PLUM_TRACE") == "1":
    install()
