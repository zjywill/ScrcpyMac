"""Minimal MCP stdio client, used to drive the Go phone-agent binary.

Deliberately hand-rolled rather than built on the Python `mcp` package: the
whole point of the parity harness is to observe what the Go server actually
puts on the wire. A second SDK in the path would normalise away exactly the
details we are checking (key order, content-block shapes, _meta round-trips).

Framing is newline-delimited JSON-RPC 2.0, which is what mcp.StdioTransport
speaks. Every stdout line is asserted to be a JSON-RPC message: the Go server's
one inviolable rule is that stdout carries protocol and nothing else, so a
stray log line is a harness failure, not something to skip past.
"""

from __future__ import annotations

import json
import os
import queue
import signal
import subprocess
import threading
import time
from typing import Any, Optional

PROTOCOL_VERSION = "2025-06-18"


class MCPError(RuntimeError):
    pass


class StdioMCPClient:
    """One `phone-agent mcp` process, spoken to over stdio."""

    def __init__(
        self,
        argv: list[str],
        env: Optional[dict[str, str]] = None,
        cwd: Optional[str] = None,
        stderr_path: Optional[str] = None,
        read_timeout: float = 120.0,
    ) -> None:
        self.argv = argv
        self.env = env
        self.cwd = cwd
        self.stderr_path = stderr_path
        self.read_timeout = read_timeout
        self._id = 0
        self._proc: Optional[subprocess.Popen] = None
        self._lines: "queue.Queue[Optional[str]]" = queue.Queue()
        self._reader: Optional[threading.Thread] = None
        self._stderr_file = None
        self.stdout_lines: list[str] = []
        self.server_info: dict[str, Any] = {}
        self.capabilities: dict[str, Any] = {}

    # -- lifecycle ---------------------------------------------------------

    def __enter__(self) -> "StdioMCPClient":
        self.start()
        self.initialize()
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def start(self) -> None:
        if self.stderr_path:
            self._stderr_file = open(self.stderr_path, "wb")
            stderr = self._stderr_file
        else:
            stderr = subprocess.DEVNULL
        self._proc = subprocess.Popen(
            self.argv,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=stderr,
            env=self.env,
            cwd=self.cwd,
            bufsize=0,
        )
        self._reader = threading.Thread(target=self._pump, daemon=True)
        self._reader.start()

    def _pump(self) -> None:
        assert self._proc is not None and self._proc.stdout is not None
        for raw in self._proc.stdout:
            self._lines.put(raw.decode("utf-8", "replace").rstrip("\n"))
        self._lines.put(None)

    def close(self) -> None:
        """Close stdin, wait briefly, then escalate. Never leaves an orphan."""
        if self._proc is None:
            return
        try:
            if self._proc.stdin:
                self._proc.stdin.close()
        except OSError:
            pass
        try:
            self._proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self._proc.send_signal(signal.SIGTERM)
            try:
                self._proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._proc.kill()
                self._proc.wait(timeout=5)
        if self._stderr_file is not None:
            self._stderr_file.close()
            self._stderr_file = None
        self._proc = None

    # -- transport ---------------------------------------------------------

    def _write(self, message: dict[str, Any]) -> None:
        assert self._proc is not None and self._proc.stdin is not None
        payload = json.dumps(message, ensure_ascii=False).encode("utf-8") + b"\n"
        self._proc.stdin.write(payload)
        self._proc.stdin.flush()

    def _read_message(self, timeout: float) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise MCPError(f"timed out after {timeout}s waiting for a response")
            try:
                line = self._lines.get(timeout=remaining)
            except queue.Empty:
                raise MCPError(f"timed out after {timeout}s waiting for a response")
            if line is None:
                raise MCPError("server closed stdout before responding")
            if not line.strip():
                continue
            self.stdout_lines.append(line)
            try:
                msg = json.loads(line)
            except json.JSONDecodeError as exc:
                # Contract violation, not a parsing inconvenience.
                raise MCPError(f"stdout carried a non-JSON-RPC line: {line!r}") from exc
            if not isinstance(msg, dict) or msg.get("jsonrpc") != "2.0":
                raise MCPError(f"stdout carried a non-JSON-RPC message: {line!r}")
            return msg

    def request(self, method: str, params: Any = None, timeout: Optional[float] = None) -> Any:
        self._id += 1
        want = self._id
        msg: dict[str, Any] = {"jsonrpc": "2.0", "id": want, "method": method}
        if params is not None:
            msg["params"] = params
        self._write(msg)
        budget = self.read_timeout if timeout is None else timeout
        deadline = time.monotonic() + budget
        while True:
            reply = self._read_message(max(0.01, deadline - time.monotonic()))
            if reply.get("id") != want:
                # Notifications and unrelated ids: keep reading.
                continue
            if "error" in reply:
                raise MCPError(f"{method} returned a JSON-RPC error: {reply['error']}")
            return reply.get("result")

    def notify(self, method: str, params: Any = None) -> None:
        msg: dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        self._write(msg)

    # -- MCP -------------------------------------------------------------

    def initialize(self) -> None:
        result = self.request(
            "initialize",
            {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": {"name": "scrcpymac-parity", "version": "1"},
            },
            timeout=30,
        )
        self.server_info = result.get("serverInfo", {}) or {}
        self.capabilities = result.get("capabilities", {}) or {}
        self.instructions = result.get("instructions", "")
        self.notify("notifications/initialized")

    def list_tools(self) -> list[dict[str, Any]]:
        return self.request("tools/list", {}, timeout=30).get("tools", [])

    def call_tool(self, name: str, arguments: Optional[dict[str, Any]] = None,
                  timeout: Optional[float] = None) -> dict[str, Any]:
        return self.request(
            "tools/call",
            {"name": name, "arguments": arguments or {}},
            timeout=timeout,
        )

    def read_resource(self, uri: str) -> dict[str, Any]:
        return self.request("resources/read", {"uri": uri}, timeout=30)

    # -- helpers ----------------------------------------------------------

    @staticmethod
    def text_blocks(result: dict[str, Any]) -> list[str]:
        return [c.get("text", "") for c in result.get("content", []) if c.get("type") == "text"]

    @staticmethod
    def image_blocks(result: dict[str, Any]) -> list[dict[str, Any]]:
        return [c for c in result.get("content", []) if c.get("type") == "image"]

    def stderr_text(self) -> str:
        if not self.stderr_path or not os.path.exists(self.stderr_path):
            return ""
        with open(self.stderr_path, "rb") as fh:
            return fh.read().decode("utf-8", "replace")
