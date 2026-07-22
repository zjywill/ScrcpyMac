"""Ordered JSON comparison with explicit volatility rules.

Two things make this more than `assert a == b`:

1. **Key order is contract.** The Python emits `json.dumps(payload,
   ensure_ascii=False, indent=2)` over insertion-ordered dicts. `dict ==` and
   any structural comparison that goes through a plain dict throws that away,
   which is precisely the failure mode the Go port is most exposed to
   (`map[string]any` marshals alphabetically). So objects are parsed into
   ordered pairs and compared position by position.

2. **Volatility must be declared, never inferred.** Every field a run is
   allowed to differ on is named by path. Anything not named is compared, and a
   difference is a failure. Ignored values are still reported so a reviewer can
   see what they were.

Path syntax: `$`, `.key`, `[index]`. Patterns may use `*` for one whole
segment: `$.checks[*].detail`, `$.verification.attempts[*].change_score`.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from typing import Any, Iterable, Optional

# Rule verbs.
IGNORE = "ignore"    # presence and position compared, value not
EXPECTED = "expected"  # value may differ; recorded as an intentional deviation
DROP = "drop"        # key removed from both sides before diffing


class Pairs(list):
    """An ordered JSON object: a list of (key, value) with dict-ish helpers."""

    def keys(self) -> list[str]:
        return [k for k, _ in self]

    def get(self, key: str, default: Any = None) -> Any:
        for k, v in self:
            if k == key:
                return v
        return default

    def has(self, key: str) -> bool:
        return any(k == key for k, _ in self)

    def without(self, keys: Iterable[str]) -> "Pairs":
        drop = set(keys)
        return Pairs((k, v) for k, v in self if k not in drop)


def parse(text: str) -> Any:
    """Parse JSON preserving object key order."""
    return json.loads(text, object_pairs_hook=lambda pairs: Pairs(pairs))


def dumps(value: Any) -> str:
    """Re-serialise exactly the way actions.json_result does."""
    return json.dumps(_plain(value), ensure_ascii=False, indent=2)


def _plain(value: Any) -> Any:
    if isinstance(value, Pairs):
        return {k: _plain(v) for k, v in value}
    if isinstance(value, list):
        return [_plain(v) for v in value]
    return value


# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------


def _child(path: str, seg: str) -> str:
    return f"{path}.{seg}"


def _index(path: str, i: int) -> str:
    return f"{path}[{i}]"


_SEG = re.compile(r"\.([^.\[\]]+)|\[([^\]]+)\]")


def _segments(path: str) -> list[str]:
    return [a if a is not None else b for a, b in _SEG.findall(path)]


def _matches(pattern: str, path: str) -> bool:
    p, q = _segments(pattern), _segments(path)
    if len(p) != len(q):
        return False
    return all(a == "*" or a == b for a, b in zip(p, q))


class Rules:
    """Path patterns to verbs."""

    def __init__(self, spec: Optional[dict[str, str]] = None) -> None:
        self.spec = dict(spec or {})

    def verb(self, path: str) -> Optional[str]:
        for pattern, verb in self.spec.items():
            if _matches(pattern, path):
                return verb
        return None


# ---------------------------------------------------------------------------
# Diffing
# ---------------------------------------------------------------------------


@dataclass
class Diff:
    kind: str
    path: str
    py: Any = None
    go: Any = None
    note: str = ""

    def render(self) -> str:
        head = f"[{self.kind}] {self.path}"
        if self.note:
            head += f"  ({self.note})"
        if self.kind in ("value", "type", "list-length", "key-order"):
            head += f"\n      python: {_short(self.py)}\n      go    : {_short(self.go)}"
        elif self.kind == "key-missing-go":
            head += f"\n      python: {_short(self.py)}\n      go    : <absent>"
        elif self.kind == "key-missing-py":
            head += f"\n      python: <absent>\n      go    : {_short(self.go)}"
        return head


@dataclass
class Report:
    diffs: list[Diff] = field(default_factory=list)
    ignored: list[Diff] = field(default_factory=list)
    expected: list[Diff] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.diffs

    def extend(self, other: "Report") -> None:
        self.diffs.extend(other.diffs)
        self.ignored.extend(other.ignored)
        self.expected.extend(other.expected)


def _short(value: Any, limit: int = 300) -> str:
    if isinstance(value, str):
        text = repr(value)
    else:
        text = json.dumps(_plain(value), ensure_ascii=False)
    return text if len(text) <= limit else text[: limit - 3] + "..."


def diff(py: Any, go: Any, rules: Optional[Rules] = None, path: str = "$") -> Report:
    rules = rules or Rules()
    report = Report()
    _diff(py, go, rules, path, report)
    return report


def _diff(py: Any, go: Any, rules: Rules, path: str, report: Report) -> None:
    verb = rules.verb(path)
    if verb == IGNORE:
        if py != go:
            report.ignored.append(Diff("ignored", path, py, go))
        return
    if verb == EXPECTED:
        if py != go:
            report.expected.append(Diff("expected", path, py, go))
        return

    if isinstance(py, Pairs) and isinstance(go, Pairs):
        _diff_object(py, go, rules, path, report)
        return
    if isinstance(py, list) and isinstance(go, list):
        if len(py) != len(go):
            report.diffs.append(Diff("list-length", path, len(py), len(go)))
        for i in range(min(len(py), len(go))):
            _diff(py[i], go[i], rules, _index(path, i), report)
        return
    if type(py) is not type(go) and not (
        isinstance(py, (int, float)) and isinstance(go, (int, float))
    ):
        report.diffs.append(
            Diff("type", path, py, go, f"{type(py).__name__} vs {type(go).__name__}")
        )
        return
    if py != go:
        report.diffs.append(Diff("value", path, py, go))
        return
    # Equal values of the same type: also catch int/float rendering drift,
    # e.g. Python 0.0 against Go 0. json.dumps renders these differently and
    # the raw-text check below would catch it, but naming it here is clearer.
    if isinstance(py, bool) or isinstance(go, bool):
        return
    if isinstance(py, float) != isinstance(go, float):
        report.diffs.append(
            Diff("type", path, py, go, "float vs int rendering — json output differs")
        )


def _diff_object(py: Pairs, go: Pairs, rules: Rules, path: str, report: Report) -> None:
    # Drop declared one-sided keys from both sides first.
    def keep(container: Pairs) -> Pairs:
        return Pairs(
            (k, v) for k, v in container if rules.verb(_child(path, k)) != DROP
        )

    py, go = keep(py), keep(go)

    py_keys, go_keys = py.keys(), go.keys()
    if py_keys != go_keys:
        if set(py_keys) == set(go_keys):
            report.diffs.append(Diff("key-order", path, py_keys, go_keys))
        else:
            for k in py_keys:
                if k not in go_keys:
                    report.diffs.append(Diff("key-missing-go", _child(path, k), py.get(k)))
            for k in go_keys:
                if k not in py_keys:
                    report.diffs.append(Diff("key-missing-py", _child(path, k), None, go.get(k)))
            common_py = [k for k in py_keys if k in go_keys]
            common_go = [k for k in go_keys if k in py_keys]
            if common_py != common_go:
                report.diffs.append(Diff("key-order", path, common_py, common_go))

    for k in py_keys:
        if k in go_keys:
            _diff(py.get(k), go.get(k), rules, _child(path, k), report)


# ---------------------------------------------------------------------------
# Raw-text checks
# ---------------------------------------------------------------------------


def masked(value: Any, rules: Rules, path: str = "$") -> Any:
    """Replace every ignored/expected leaf with a sentinel, drop DROP keys.

    Re-serialising two masked trees with the same encoder and comparing the
    text is a second, independent check on key order and value equality.
    """
    verb = rules.verb(path)
    if verb in (IGNORE, EXPECTED):
        return "<masked>"
    if isinstance(value, Pairs):
        out = Pairs()
        for k, v in value:
            child = _child(path, k)
            if rules.verb(child) == DROP:
                continue
            out.append((k, masked(v, rules, child)))
        return out
    if isinstance(value, list):
        return [masked(v, rules, _index(path, i)) for i, v in enumerate(value)]
    return value


def format_checks(raw: str) -> list[str]:
    """Formatting properties json_result guarantees, independent of content."""
    problems = []
    if raw.endswith("\n"):
        problems.append("trailing newline (json.dumps emits none)")
    html = [e for e in ("\\u003c", "\\u003e", "\\u0026") if e in raw.lower()]
    if html:
        problems.append(f"HTML-escaped <, > or & (SetEscapeHTML(false) missing): {html}")
    # Both encoders escape C0 control characters; anything above U+001F escaped
    # means ensure_ascii/EscapeHTML semantics drifted.
    escapes = sorted(
        e for e in set(re.findall(r"\\u[0-9a-fA-F]{4}", raw)) if int(e[2:], 16) > 0x1F
    )
    escapes = [e for e in escapes if e.lower() not in ("\\u003c", "\\u003e", "\\u0026")]
    if escapes:
        problems.append(f"non-ASCII escaped instead of raw UTF-8: {escapes[:5]}")
    lines = raw.split("\n")
    if len(lines) > 1:
        indents = {len(l) - len(l.lstrip(" ")) for l in lines[1:] if l.strip()}
        if any(i % 2 for i in indents):
            problems.append(f"indent is not a multiple of 2: {sorted(indents)}")
        if "\t" in raw:
            problems.append("tab characters in the indent")
    return problems
