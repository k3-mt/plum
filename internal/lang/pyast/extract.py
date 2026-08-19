"""PLUM's Python extractor.

Runs under the repository's own interpreter and prints one JSON document on
stdout describing a single source file. Python ships its own parser, so this
needs no grammar, no bindings and no cgo — `ast` gives exact structure, and
`tokenize` gives back the comments `ast` throws away.

Read from stdin: the file source. Argument: the repo-relative path (used only
for messages; the Go side owns SymbolID construction).
"""

from __future__ import annotations

import ast
import io
import json
import sys
import tokenize

# ---------------------------------------------------------------- comments


def collect_comments(src: str):
    """Every comment with its line span, plus a line -> block-end index.

    Contiguous runs of full-line comments are merged into one block, because a
    rationale comment above a call is written as a paragraph, not as lines.
    """
    comments = []
    try:
        for tok in tokenize.generate_tokens(io.StringIO(src).readline):
            if tok.type == tokenize.COMMENT:
                comments.append({
                    "text": tok.string.lstrip("#").strip(),
                    "line": tok.start[0],
                    "col": tok.start[1],
                    "own_line": src.splitlines()[tok.start[0] - 1][:tok.start[1]].strip() == "",
                })
    except (tokenize.TokenError, IndentationError, SyntaxError):
        pass  # a partial file still yields structure from ast below

    blocks = []
    for c in comments:
        if (blocks and c["own_line"] and blocks[-1]["own_line"]
                and c["line"] == blocks[-1]["line_end"] + 1
                and c["col"] == blocks[-1]["col"]):
            blocks[-1]["line_end"] = c["line"]
            blocks[-1]["text"] += "\n" + c["text"]
        else:
            blocks.append({
                "text": c["text"], "line_start": c["line"], "line_end": c["line"],
                "col": c["col"], "own_line": c["own_line"],
            })
    return blocks


def block_ending_at(blocks, line):
    """The contiguous own-line comment block immediately above `line`."""
    for b in blocks:
        if b["own_line"] and b["line_end"] == line - 1:
            return b["text"]
    return ""


# ---------------------------------------------------------------- signatures


def render_signature(node) -> str:
    a = node.args
    parts = []

    def render_arg(arg, default=None):
        s = arg.arg
        if arg.annotation is not None:
            s += ": " + ast.unparse(arg.annotation)
        if default is not None:
            s += "=" + ast.unparse(default)
        return s

    posonly = list(getattr(a, "posonlyargs", []))
    args = posonly + list(a.args)
    defaults = [None] * (len(args) - len(a.defaults)) + list(a.defaults)
    for i, arg in enumerate(args):
        parts.append(render_arg(arg, defaults[i]))
        if posonly and i == len(posonly) - 1:
            parts.append("/")
    if a.vararg is not None:
        parts.append("*" + render_arg(a.vararg))
    elif a.kwonlyargs:
        parts.append("*")
    for arg, default in zip(a.kwonlyargs, a.kw_defaults):
        parts.append(render_arg(arg, default))
    if a.kwarg is not None:
        parts.append("**" + render_arg(a.kwarg))

    prefix = "async def " if isinstance(node, ast.AsyncFunctionDef) else "def "
    sig = prefix + node.name + "(" + ", ".join(parts) + ")"
    if node.returns is not None:
        sig += " -> " + ast.unparse(node.returns)
    return sig


def decorators(node):
    return [ast.unparse(d) for d in getattr(node, "decorator_list", [])]


# ---------------------------------------------------------------- normalise


def normalise(src: str, start: int, end: int) -> str:
    """Token stream for a line range: comments and docstrings dropped,
    whitespace collapsed, identifiers preserved, indentation kept (it is
    syntax in Python). Reformatting must not move a fingerprint; a rename or a
    logic change must.
    """
    lines = src.splitlines(keepends=True)[start - 1:end]
    fragment = "".join(lines)
    # Dedent so a method fingerprints identically wherever it sits.
    stripped = [ln for ln in fragment.splitlines() if ln.strip()]
    pad = min((len(ln) - len(ln.lstrip()) for ln in stripped), default=0)
    fragment = "\n".join(ln[pad:] if len(ln) >= pad else ln for ln in fragment.splitlines())

    out = []
    try:
        for tok in tokenize.generate_tokens(io.StringIO(fragment).readline):
            if tok.type in (tokenize.COMMENT, tokenize.NL, tokenize.NEWLINE,
                            tokenize.ENCODING, tokenize.ENDMARKER):
                continue
            if tok.type == tokenize.INDENT:
                out.append("<indent>")
            elif tok.type == tokenize.DEDENT:
                out.append("<dedent>")
            else:
                out.append(tok.string.strip())
    except (tokenize.TokenError, IndentationError, SyntaxError):
        return " ".join(fragment.split())

    return " ".join(t for t in drop_docstring(out) if t)


def drop_docstring(tokens):
    """Remove a leading docstring: prose about the code is not the code.

    A docstring is a bare string statement at the start of the body, so it sits
    just past the `:` that opens the block (and past the INDENT marker), or at
    index 0 when the fragment is a module.
    """
    if not tokens:
        return tokens
    if tokens[0][:1] in ('"', "'"):
        return tokens[1:]
    for i, tok in enumerate(tokens[:-1]):
        if tok == ":":
            j = i + 1
            if j < len(tokens) and tokens[j] == "<indent>":
                j += 1
            if j < len(tokens) and tokens[j][:1] in ('"', "'"):
                return tokens[:j] + tokens[j + 1:]
            return tokens
    return tokens


# ---------------------------------------------------------------- walking


def qualified(stack, name):
    return ".".join([s for s in stack] + [name])


def callee_name(node):
    f = node.func
    parts = []
    while isinstance(f, ast.Attribute):
        parts.append(f.attr)
        f = f.value
    if isinstance(f, ast.Name):
        parts.append(f.id)
    elif isinstance(f, ast.Call):
        parts.append("<call>")
    else:
        return ""
    return ".".join(reversed(parts))


def literal_str(node):
    return node.value if isinstance(node, ast.Constant) and isinstance(node.value, str) else None


def kwarg(call, name):
    for kw in call.keywords:
        if kw.arg == name:
            return kw
    return None


class Extractor(ast.NodeVisitor):
    def __init__(self, src, path):
        self.src = src
        self.path = path
        self.lines = src.splitlines()
        self.blocks = collect_comments(src)
        self.symbols = []
        self.surface = []
        self.risks = []
        self.edges = []
        self.stack = []          # enclosing class/function names
        self.current = None      # symbol dict being filled
        self.globals_written = set()
        self.module_assigns = {}

    # -- helpers ---------------------------------------------------------

    def add_risk(self, kind, symbol, line, note):
        self.risks.append({"kind": kind, "symbol": symbol, "line": line, "note": note})

    def exported_class_path(self):
        """True when every enclosing scope is an exported class."""
        return bool(self.stack) and all(not part.startswith("_") for part in self.stack)

    def symbol_at(self, line):
        best, span = "", 1 << 30
        for s in self.symbols:
            if s["line_start"] <= line <= s["line_end"] and s["line_end"] - s["line_start"] < span:
                best, span = s["name"], s["line_end"] - s["line_start"]
        return best

    def comments_within(self, start, end):
        return [{"text": b["text"], "line_start": b["line_start"], "line_end": b["line_end"]}
                for b in self.blocks if start <= b["line_start"] and b["line_end"] <= end]

    # -- declarations ----------------------------------------------------

    def visit_ClassDef(self, node):
        name = qualified(self.stack, node.name)
        sym = {
            "name": name, "kind": "class",
            "line_start": min([node.lineno] + [d.lineno for d in node.decorator_list]),
            "line_end": node.end_lineno,
            "signature": "class " + node.name + (
                "(" + ", ".join(ast.unparse(b) for b in node.bases) + ")" if node.bases else ""),
            "doc": ast.get_docstring(node) or "",
            "exported": not node.name.startswith("_"),
            "decorators": decorators(node),
        }
        self.finish(sym, node)
        if not self.stack and sym["exported"]:
            self.surface.append({"kind": "export", "name": node.name,
                                 "signature": sym["signature"], "symbol": name})
        self.stack.append(node.name)
        self.generic_visit(node)
        self.stack.pop()

    def visit_FunctionDef(self, node):
        self._function(node)

    def visit_AsyncFunctionDef(self, node):
        self._function(node)

    def _function(self, node):
        name = qualified(self.stack, node.name)
        kind = "method" if self.stack and "." not in name[:-len(node.name) - 1 or None] else "func"
        kind = "method" if self.stack else "func"
        sym = {
            "name": name, "kind": kind,
            "line_start": min([node.lineno] + [d.lineno for d in node.decorator_list]),
            "line_end": node.end_lineno,
            "signature": render_signature(node),
            "doc": ast.get_docstring(node) or "",
            "exported": not node.name.startswith("_"),
            "decorators": decorators(node),
        }
        self.finish(sym, node)
        # A method on an exported class is reachable surface, exactly as an
        # exported method on an exported type is in Go. A signature change here
        # is what silently breaks callers nobody looked at.
        if sym["exported"] and (not self.stack or self.exported_class_path()):
            self.surface.append({"kind": "export", "name": name,
                                 "signature": sym["signature"], "symbol": name})

        # Routes are declared by decorator in every mainstream Python framework.
        for dec in node.decorator_list:
            call = dec if isinstance(dec, ast.Call) else None
            raw = ast.unparse(dec)
            low = raw.lower()
            if any(h in low for h in ("route", ".get(", ".post(", ".put(", ".delete(", ".patch(")):
                path = literal_str(call.args[0]) if call and call.args else None
                if path and path.startswith("/"):
                    verb = ""
                    for v in ("get", "post", "put", "delete", "patch"):
                        if "." + v + "(" in low:
                            verb = v.upper() + " "
                    self.surface.append({"kind": "route", "name": verb + path,
                                         "signature": raw, "symbol": name})

        self.mutable_defaults(node, name)
        self.stack.append(node.name)
        prev, self.current = self.current, sym
        self.generic_visit(node)
        self.current = prev
        self.stack.pop()

    def finish(self, sym, node):
        sym["comments"] = self.comments_within(sym["line_start"], sym["line_end"])
        sym["call_sites"] = []
        sym["norm"] = normalise(self.src, sym["line_start"], sym["line_end"])
        self.symbols.append(sym)

    def mutable_defaults(self, node, name):
        for default in list(node.args.defaults) + [d for d in node.args.kw_defaults if d]:
            if isinstance(default, (ast.List, ast.Dict, ast.Set)):
                self.add_risk("mutable_default_arg", name, default.lineno,
                              "a mutable default is created once at definition time and shared by every call")
            if isinstance(default, ast.Call) and callee_name(default) in ("list", "dict", "set"):
                self.add_risk("mutable_default_arg", name, default.lineno,
                              "a mutable default is created once at definition time and shared by every call")

    # -- module-level state ----------------------------------------------

    def visit_Assign(self, node):
        if not self.stack:
            for target in node.targets:
                if isinstance(target, ast.Name):
                    self.module_assigns[target.id] = (node.lineno, node.value)
        self.generic_visit(node)

    def visit_Global(self, node):
        for name in node.names:
            self.globals_written.add(name)
        self.generic_visit(node)

    # -- calls -----------------------------------------------------------

    def visit_Call(self, node):
        name = callee_name(node)
        line = node.lineno
        owner = self.current["name"] if self.current else self.symbol_at(line)

        if self.current is not None and name:
            self.current["call_sites"].append({
                "callee_raw": name, "line": line,
                "rationale": block_ending_at(self.blocks, line),
            })
            self.edges.append({"from": self.current["name"], "to": name})

        short = name.rsplit(".", 1)[-1]

        # Environment variables are the seam between config and code.
        if name in ("os.getenv", "os.environ.get", "environ.get", "getenv"):
            v = literal_str(node.args[0]) if node.args else None
            if v:
                self.surface.append({"kind": "env_var", "name": v, "signature": name, "symbol": owner})

        if short == "add_argument":
            v = literal_str(node.args[0]) if node.args else None
            if v and v.startswith("-"):
                self.surface.append({"kind": "cli_flag", "name": v, "signature": name, "symbol": owner})

        if name.split(".")[0] in ("requests", "httpx") and short in ("get", "post", "put", "delete", "patch", "request", "head"):
            if kwarg(node, "timeout") is None:
                self.add_risk("network_without_timeout", owner, line,
                              name + " has no timeout= — the call can hang for as long as the other side likes")
        if name in ("urllib.request.urlopen", "request.urlopen", "urlopen") and kwarg(node, "timeout") is None:
            self.add_risk("network_without_timeout", owner, line, "urlopen with no timeout=")

        if name.startswith("subprocess.") or short in ("run", "Popen", "check_output", "call"):
            if name.startswith("subprocess."):
                if kwarg(node, "timeout") is None:
                    self.add_risk("subprocess_without_timeout", owner, line,
                                  name + " has no timeout= — the child can outlive its caller")
                shell = kwarg(node, "shell")
                if shell is not None and isinstance(shell.value, ast.Constant) and shell.value.value is True:
                    self.add_risk("shell_injection_surface", owner, line,
                                  name + " with shell=True — the argument string is interpreted by a shell")

        if short == "Thread" and name.split(".")[0] in ("threading", "Thread"):
            self.add_risk("unsynchronised_thread", owner, line,
                          "a bare Thread with no join in sight — completion is unobservable")

        if name in ("eval", "exec"):
            self.add_risk("dynamic_execution", owner, line,
                          name + "() executes whatever it is handed")

        if short == "read" and not node.args:
            self.add_risk("unbounded_read", owner, line,
                          "read() with no size reads the whole stream into memory")

        self.generic_visit(node)

    # -- exception handling ----------------------------------------------

    def visit_ExceptHandler(self, node):
        owner = self.current["name"] if self.current else self.symbol_at(node.lineno)
        body = [s for s in node.body if not isinstance(s, ast.Pass)]
        if not body:
            what = ast.unparse(node.type) if node.type else "everything"
            self.add_risk("swallowed_error", owner, node.lineno,
                          "except " + what + " with an empty body — the failure is observed and discarded")
        if node.type is None:
            self.add_risk("bare_except", owner, node.lineno,
                          "a bare except also catches KeyboardInterrupt and SystemExit")
        self.generic_visit(node)


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else "<stdin>"
    src = sys.stdin.read()
    try:
        tree = ast.parse(src)
    except SyntaxError as e:
        json.dump({"error": "syntax error at line %s: %s" % (e.lineno, e.msg)}, sys.stdout)
        return

    ex = Extractor(src, path)
    ex.visit(tree)

    # Module-level state, judged the way the Go adapter judges package vars: a
    # name is only shared *mutable* state if something can write it.
    for name, (line, value) in ex.module_assigns.items():
        if name.startswith("__"):
            continue
        mutable_literal = isinstance(value, (ast.List, ast.Dict, ast.Set))
        written = name in ex.globals_written
        exported = not name.startswith("_")
        kind = "const" if name.isupper() and not mutable_literal and not written else "var"
        ex.symbols.append({
            "name": name, "kind": kind, "line_start": line, "line_end": line,
            "signature": kind + " " + name + " = " + ast.unparse(value)[:80],
            "doc": block_ending_at(ex.blocks, line), "exported": exported,
            "decorators": [], "comments": [], "call_sites": [],
            "norm": normalise(src, line, line),
        })
        if written:
            ex.risks.append({"kind": "module_level_state", "symbol": name, "line": line,
                             "note": "module-level " + name + " is rebound with `global` — shared across every caller and every test"})
        elif mutable_literal:
            ex.risks.append({"kind": "module_level_state", "symbol": name, "line": line,
                             "note": "module-level mutable " + type(value).__name__.lower() + " " + name + " — any importer can mutate it in place"})
        if exported and not ex.stack:
            ex.surface.append({"kind": "export", "name": name, "signature": ex.symbols[-1]["signature"], "symbol": name})

    json.dump({
        "symbols": ex.symbols,
        "comments": [{"text": b["text"], "line_start": b["line_start"], "line_end": b["line_end"]} for b in ex.blocks],
        "surface": ex.surface,
        "risks": ex.risks,
        "edges": ex.edges,
    }, sys.stdout)


if __name__ == "__main__":
    main()
