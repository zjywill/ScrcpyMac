"""Standalone scrcpy runtime owned entirely by the Codex plugin."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
import socket
import socketserver
import struct
import subprocess
import threading
import time
from collections import deque
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional
from urllib.parse import parse_qs, urlparse

from phone_agent.adb import AdbClient, AdbError

SCRCPY_VERSION = "3.3.4"
DEVICE_NAME_LENGTH = 64
FRAME_HEADER_LENGTH = 12
PACKET_FLAG_CONFIG = 1 << 63
PACKET_FLAG_KEY_FRAME = 1 << 62
PACKET_PTS_MASK = (1 << 62) - 1
H264_CODEC_ID = 0x68323634

WS_PACKET_VERSION = 1
WS_FLAG_CONFIG = 1
WS_FLAG_KEY_FRAME = 2

KEYCODES = {
    "back": 4,
    "home": 3,
    "recents": 187,
    "enter": 66,
    "delete": 67,
    "tab": 61,
    "menu": 82,
    "power": 26,
    "volume_up": 24,
    "volume_down": 25,
}


class ScrcpyRuntimeError(RuntimeError):
    pass


@dataclass(frozen=True)
class ScrcpyVideoMeta:
    serial: str
    device_name: str
    codec_id: int
    width: int
    height: int
    native_width: int
    native_height: int
    max_fps: int
    resolution_percent: int


def resolve_scrcpy_server_path() -> str:
    requested = os.environ.get("SCRCPY_SERVER_PATH")
    candidates: list[Path] = []
    if requested:
        candidates.append(Path(requested).expanduser())

    root = os.environ.get("PHONE_AGENT_ROOT")
    if root:
        plugin_root = Path(root)
        candidates.extend(
            [
                plugin_root / "share" / "scrcpy-server",
                plugin_root / "bin" / "scrcpy-server",
                plugin_root / "bin" / "darwin" / "share" / "scrcpy-server",
            ]
        )

    candidates.extend(
        [
            Path("/opt/homebrew/share/scrcpy/scrcpy-server"),
            Path("/usr/local/share/scrcpy/scrcpy-server"),
        ]
    )
    for candidate in candidates:
        if candidate.is_file():
            return str(candidate)
    raise ScrcpyRuntimeError(
        "scrcpy-server is missing from the plugin. Reinstall the plugin or set "
        "SCRCPY_SERVER_PATH."
    )


def _recv_exact(connection: socket.socket, size: int) -> bytes:
    chunks = bytearray()
    while len(chunks) < size:
        chunk = connection.recv(size - len(chunks))
        if not chunk:
            raise ScrcpyRuntimeError("short read from scrcpy-server")
        chunks.extend(chunk)
    return bytes(chunks)


def _split_annex_b(data: bytes) -> list[bytes]:
    starts: list[tuple[int, int]] = []
    index = 0
    while index + 3 <= len(data):
        if data[index : index + 4] == b"\x00\x00\x00\x01":
            starts.append((index, 4))
            index += 4
            continue
        if data[index : index + 3] == b"\x00\x00\x01":
            starts.append((index, 3))
            index += 3
            continue
        index += 1

    nal_units: list[bytes] = []
    for item_index, (start, prefix_length) in enumerate(starts):
        end = starts[item_index + 1][0] if item_index + 1 < len(starts) else len(data)
        payload = data[start + prefix_length : end]
        if payload:
            nal_units.append(payload)
    return nal_units


def _codec_from_config(data: bytes) -> str:
    for nal_unit in _split_annex_b(data):
        if nal_unit and (nal_unit[0] & 0x1F) == 7 and len(nal_unit) >= 4:
            return f"avc1.{nal_unit[1]:02X}{nal_unit[2]:02X}{nal_unit[3]:02X}"
    return "avc1.42E01E"


def _ws_frame(opcode: int, payload: bytes) -> bytes:
    first = 0x80 | (opcode & 0x0F)
    length = len(payload)
    if length < 126:
        header = struct.pack(">BB", first, length)
    elif length <= 0xFFFF:
        header = struct.pack(">BBH", first, 126, length)
    else:
        header = struct.pack(">BBQ", first, 127, length)
    return header + payload


class _WebSocketClient:
    def __init__(self, connection: socket.socket):
        self.connection = connection
        self.send_lock = threading.Lock()
        self.closed = False

    def send_text(self, payload: dict[str, Any]) -> None:
        self._send(0x1, json.dumps(payload, separators=(",", ":")).encode("utf-8"))

    def send_binary(self, payload: bytes) -> None:
        self._send(0x2, payload)

    def send_pong(self, payload: bytes) -> None:
        self._send(0xA, payload)

    def _send(self, opcode: int, payload: bytes) -> None:
        frame = _ws_frame(opcode, payload)
        with self.send_lock:
            if self.closed:
                raise OSError("WebSocket client is closed")
            self.connection.sendall(frame)

    def read_until_close(self) -> None:
        self.connection.settimeout(30)
        while not self.closed:
            try:
                head = _recv_exact(self.connection, 2)
            except socket.timeout:
                continue
            first, second = head
            opcode = first & 0x0F
            masked = bool(second & 0x80)
            length = second & 0x7F
            if length == 126:
                length = struct.unpack(">H", _recv_exact(self.connection, 2))[0]
            elif length == 127:
                length = struct.unpack(">Q", _recv_exact(self.connection, 8))[0]
            mask = _recv_exact(self.connection, 4) if masked else b""
            payload = _recv_exact(self.connection, length) if length else b""
            if masked:
                payload = bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload))
            if opcode == 0x8:
                return
            if opcode == 0x9:
                self.send_pong(payload)

    def close(self) -> None:
        with self.send_lock:
            if self.closed:
                return
            self.closed = True
            try:
                self.connection.sendall(_ws_frame(0x8, b""))
            except OSError:
                pass
            try:
                self.connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            self.connection.close()


class _ThreadingLoopbackServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, runtime: "ScrcpyRuntime"):
        self.runtime = runtime
        super().__init__(("127.0.0.1", 0), _WebSocketHandler)


class _WebSocketHandler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        server = self.server
        if not isinstance(server, _ThreadingLoopbackServer):
            return
        connection = self.request
        if not isinstance(connection, socket.socket):
            return

        connection.settimeout(5)
        raw = bytearray()
        while b"\r\n\r\n" not in raw and len(raw) < 16_384:
            chunk = connection.recv(4096)
            if not chunk:
                return
            raw.extend(chunk)

        try:
            header_text = bytes(raw).decode("latin-1")
            lines = header_text.split("\r\n")
            method, target, _version = lines[0].split(" ", 2)
            headers = {
                key.strip().lower(): value.strip()
                for line in lines[1:]
                if ":" in line
                for key, value in [line.split(":", 1)]
            }
            parsed = urlparse(target)
            token = parse_qs(parsed.query).get("token", [""])[0]
            websocket_key = headers.get("sec-websocket-key", "")
            if (
                method != "GET"
                or parsed.path != "/stream"
                or not websocket_key
                or headers.get("upgrade", "").lower() != "websocket"
                or not server.runtime.accepts_stream_token(token)
            ):
                connection.sendall(
                    b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                )
                return
        except (UnicodeDecodeError, ValueError):
            return

        accept = base64.b64encode(
            hashlib.sha1(
                (websocket_key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode(
                    "ascii"
                )
            ).digest()
        ).decode("ascii")
        response = (
            "HTTP/1.1 101 Switching Protocols\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Accept: {accept}\r\n"
            "\r\n"
        )
        connection.sendall(response.encode("ascii"))

        client = _WebSocketClient(connection)
        if not server.runtime.add_stream_client(token, client):
            client.close()
            return
        try:
            client.read_until_close()
        except (OSError, ScrcpyRuntimeError):
            pass
        finally:
            server.runtime.remove_stream_client(client)
            client.close()


class ScrcpyRuntime:
    """Owns one plugin-local scrcpy server, stream and control session."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._control_send_lock = threading.Lock()
        self._state = "idle"
        self._error = ""
        self._stream_id = 0
        self._token = ""
        self._meta: Optional[ScrcpyVideoMeta] = None
        self._adb: Optional[AdbClient] = None
        self._forward_port = 0
        self._server_process: Optional[subprocess.Popen[bytes]] = None
        self._video_socket: Optional[socket.socket] = None
        self._control_socket: Optional[socket.socket] = None
        self._video_thread: Optional[threading.Thread] = None
        self._loopback: Optional[_ThreadingLoopbackServer] = None
        self._loopback_thread: Optional[threading.Thread] = None
        self._clients: set[_WebSocketClient] = set()
        self._config_packet: Optional[bytes] = None
        self._key_packet: Optional[bytes] = None
        self._codec = "avc1.42E01E"
        self._frame_times: deque[float] = deque(maxlen=180)
        self._frame_count = 0

    def backend_name(self) -> str:
        with self._lock:
            return "plugin-h264" if self._state == "streaming" else "adb"

    def stream_connect_domains(self) -> list[str]:
        self._ensure_loopback()
        with self._lock:
            if self._loopback is None:
                raise ScrcpyRuntimeError("plugin loopback stream is unavailable")
            port = int(self._loopback.server_address[1])
        return [
            f"http://127.0.0.1:{port}",
            f"ws://127.0.0.1:{port}",
            f"http://localhost:{port}",
            f"ws://localhost:{port}",
            "http://127.0.0.1:*",
            "ws://127.0.0.1:*",
            "http://localhost:*",
            "ws://localhost:*",
        ]

    def accepts_stream_token(self, token: str) -> bool:
        with self._lock:
            return (
                bool(token)
                and secrets.compare_digest(token, self._token)
                and self._state == "streaming"
            )

    def is_active(self, serial: str = "") -> bool:
        with self._lock:
            return (
                self._state == "streaming"
                and self._meta is not None
                and (not serial or self._meta.serial == serial)
            )

    def status(self) -> dict[str, Any]:
        with self._lock:
            meta = self._meta
            loopback_port = self._loopback.server_address[1] if self._loopback else 0
            fps = 0.0
            if len(self._frame_times) >= 2:
                elapsed = self._frame_times[-1] - self._frame_times[0]
                if elapsed > 0:
                    fps = (len(self._frame_times) - 1) / elapsed
            payload: dict[str, Any] = {
                "ok": self._state != "error",
                "state": self._state,
                "backend": "plugin-h264" if self._state == "streaming" else "adb",
                "encoding": "H.264" if self._state == "streaming" else "JPEG",
                "error": self._error,
                "fps": round(fps, 1),
                "frames": self._frame_count,
            }
            if meta:
                payload.update(
                    {
                        "serial": meta.serial,
                        "deviceName": meta.device_name,
                        "deviceWidth": meta.native_width,
                        "deviceHeight": meta.native_height,
                        "frameWidth": meta.width,
                        "frameHeight": meta.height,
                        "maxFps": meta.max_fps,
                        "resolutionPercent": meta.resolution_percent,
                        "codec": self._codec,
                    }
                )
            if self._state == "streaming" and loopback_port and self._token:
                payload["streamUrl"] = (
                    f"ws://127.0.0.1:{loopback_port}/stream?token={self._token}"
                )
            return payload

    def start(
        self,
        serial: str,
        *,
        max_fps: int = 60,
        resolution_percent: int = 50,
    ) -> dict[str, Any]:
        serial = serial.strip()
        if not serial:
            raise ScrcpyRuntimeError("device serial must not be empty")
        max_fps = 60 if int(max_fps) >= 45 else 30
        resolution_percent = min(100, max(25, int(resolution_percent)))

        self.stop()
        with self._lock:
            self._state = "starting"
            self._error = ""
            self._stream_id += 1
            stream_id = self._stream_id
            self._token = secrets.token_urlsafe(32)
            self._frame_times.clear()
            self._frame_count = 0

        adb = AdbClient(serial=serial)
        server_path = resolve_scrcpy_server_path()
        process: Optional[subprocess.Popen[bytes]] = None
        video_socket: Optional[socket.socket] = None
        control_socket: Optional[socket.socket] = None
        forward_port = 0
        try:
            self._ensure_loopback()
            native_width, native_height = adb.screen_size()
            max_size = max(
                320,
                round(max(native_width, native_height) * resolution_percent / 100),
            )
            adb.run(
                [
                    "push",
                    server_path,
                    "/data/local/tmp/scrcpymac-plugin-server.jar",
                ],
                timeout=60,
            )

            scid = secrets.randbelow(0x7FFF_FFFE) + 1
            socket_name = f"localabstract:scrcpy_{scid:08x}"
            forward = adb.run(["forward", "tcp:0", socket_name])
            try:
                forward_port = int((forward.stdout or "").strip())
            except ValueError as exc:
                raise ScrcpyRuntimeError(
                    f"adb did not allocate a scrcpy forward port: {forward.stdout!r}"
                ) from exc

            command = [adb.adb_path, "-s", serial, "shell"]
            command.extend(
                [
                    "CLASSPATH=/data/local/tmp/scrcpymac-plugin-server.jar",
                    "app_process",
                    "/",
                    "com.genymobile.scrcpy.Server",
                    SCRCPY_VERSION,
                    f"scid={scid:08x}",
                    "log_level=info",
                    "tunnel_forward=true",
                    "video=true",
                    "audio=false",
                    "control=true",
                    "video_codec=h264",
                    f"max_size={max_size}",
                    f"max_fps={max_fps}",
                    "video_bit_rate=4000000",
                    "clipboard_autosync=false",
                    "cleanup=false",
                ]
            )
            process = subprocess.Popen(
                command,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            self._drain_process_pipe(process.stdout)
            self._drain_process_pipe(process.stderr)

            video_socket, control_socket, meta = self._open_scrcpy_sockets(
                serial=serial,
                port=forward_port,
                process=process,
                native_width=native_width,
                native_height=native_height,
                max_fps=max_fps,
                resolution_percent=resolution_percent,
            )

            with self._lock:
                if stream_id != self._stream_id:
                    raise ScrcpyRuntimeError("scrcpy start was superseded")
                self._adb = adb
                self._forward_port = forward_port
                self._server_process = process
                self._video_socket = video_socket
                self._control_socket = control_socket
                self._meta = meta
                self._state = "streaming"
                self._error = ""

            video_thread = threading.Thread(
                target=self._pump_video,
                args=(stream_id, video_socket),
                name="scrcpymac-h264",
                daemon=True,
            )
            with self._lock:
                self._video_thread = video_thread
            video_thread.start()
            return self.status()
        except Exception as exc:
            if video_socket:
                video_socket.close()
            if control_socket:
                control_socket.close()
            if process:
                self._terminate_process(process)
            if forward_port:
                try:
                    adb.run(["forward", "--remove", f"tcp:{forward_port}"])
                except AdbError:
                    pass
            with self._lock:
                if stream_id == self._stream_id:
                    self._state = "error"
                    self._error = str(exc)
                    self._token = ""
            if isinstance(exc, (AdbError, ScrcpyRuntimeError)):
                raise ScrcpyRuntimeError(str(exc)) from exc
            raise

    def stop(self) -> dict[str, Any]:
        self._stop_resources()
        return self.status()

    def close(self) -> None:
        self._stop_resources()
        with self._lock:
            loopback = self._loopback
            loopback_thread = self._loopback_thread
            self._loopback = None
            self._loopback_thread = None
        if loopback:
            loopback.shutdown()
            loopback.server_close()
        if (
            loopback_thread
            and loopback_thread is not threading.current_thread()
            and loopback_thread.is_alive()
        ):
            loopback_thread.join(timeout=1)

    def add_stream_client(self, token: str, client: _WebSocketClient) -> bool:
        with self._lock:
            if (
                self._state != "streaming"
                or not secrets.compare_digest(token, self._token)
                or self._meta is None
            ):
                return False
            self._clients.add(client)
            ready = self.status()
            config_packet = self._config_packet
            key_packet = self._key_packet
        try:
            client.send_text({"type": "ready", **ready})
            if config_packet:
                client.send_binary(config_packet)
            if key_packet:
                client.send_binary(key_packet)
            return True
        except OSError:
            self.remove_stream_client(client)
            return False

    def remove_stream_client(self, client: _WebSocketClient) -> None:
        with self._lock:
            self._clients.discard(client)

    def tap_relative(self, x: float, y: float) -> dict[str, Any]:
        meta = self._require_control_meta()
        x = min(1.0, max(0.0, float(x)))
        y = min(1.0, max(0.0, float(y)))
        px = round(x * (meta.width - 1))
        py = round(y * (meta.height - 1))
        self._send_touch(0, px, py, buttons=True)
        self._send_touch(1, px, py, buttons=False)
        return {
            "ok": True,
            "action": "tap",
            "serial": meta.serial,
            "point": [px, py],
            "coordinateSpace": [meta.width, meta.height],
            "backend": "plugin-control",
        }

    def swipe_relative(
        self,
        x1: float,
        y1: float,
        x2: float,
        y2: float,
        *,
        duration_ms: int = 300,
    ) -> dict[str, Any]:
        meta = self._require_control_meta()
        values = [min(1.0, max(0.0, float(value))) for value in (x1, y1, x2, y2)]
        start_x = round(values[0] * (meta.width - 1))
        start_y = round(values[1] * (meta.height - 1))
        end_x = round(values[2] * (meta.width - 1))
        end_y = round(values[3] * (meta.height - 1))
        duration_ms = min(10_000, max(0, int(duration_ms)))
        steps = max(1, min(120, round(duration_ms / 16)))

        self._send_touch(0, start_x, start_y, buttons=True)
        started = time.monotonic()
        for step in range(1, steps):
            progress = step / steps
            x = round(start_x + (end_x - start_x) * progress)
            y = round(start_y + (end_y - start_y) * progress)
            self._send_touch(2, x, y, buttons=True)
            target = started + (duration_ms / 1000) * progress
            remaining = target - time.monotonic()
            if remaining > 0:
                time.sleep(remaining)
        self._send_touch(1, end_x, end_y, buttons=False)
        return {
            "ok": True,
            "action": "swipe",
            "serial": meta.serial,
            "from": [start_x, start_y],
            "to": [end_x, end_y],
            "durationMs": duration_ms,
            "backend": "plugin-control",
        }

    def key(self, name: str) -> dict[str, Any]:
        meta = self._require_control_meta()
        normalized = name.lower().strip()
        if normalized not in KEYCODES:
            raise ScrcpyRuntimeError(
                f"Unknown key {name!r}. Supported: {', '.join(KEYCODES)}"
            )
        keycode = KEYCODES[normalized]
        self._control_send(struct.pack(">BBIII", 0, 0, keycode, 0, 0))
        self._control_send(struct.pack(">BBIII", 0, 1, keycode, 0, 0))
        return {
            "ok": True,
            "action": "key",
            "key": normalized,
            "serial": meta.serial,
            "backend": "plugin-control",
        }

    def paste(self, text: str) -> dict[str, Any]:
        meta = self._require_control_meta()
        if not text:
            raise ScrcpyRuntimeError("text must not be empty")
        encoded = text.encode("utf-8")
        packet = struct.pack(">BQBI", 9, 0, 1, len(encoded)) + encoded
        self._control_send(packet)
        return {
            "ok": True,
            "action": "paste",
            "length": len(text),
            "serial": meta.serial,
            "backend": "plugin-control",
        }

    def _require_control_meta(self) -> ScrcpyVideoMeta:
        with self._lock:
            if (
                self._state != "streaming"
                or self._meta is None
                or self._control_socket is None
            ):
                raise ScrcpyRuntimeError("plugin scrcpy stream is not running")
            return self._meta

    def _send_touch(self, action: int, x: int, y: int, *, buttons: bool) -> None:
        meta = self._require_control_meta()
        pressure = 0 if action == 1 else 0xFFFF
        action_button = 0 if action == 2 else 1
        button_mask = 1 if buttons else 0
        packet = struct.pack(
            ">BBQIIHHHII",
            2,
            action,
            0xFFFFFFFFFFFFFFFF,
            x,
            y,
            meta.width,
            meta.height,
            pressure,
            action_button,
            button_mask,
        )
        self._control_send(packet)

    def _control_send(self, payload: bytes) -> None:
        with self._control_send_lock:
            with self._lock:
                connection = self._control_socket
                active = self._state == "streaming"
            if not active or connection is None:
                raise ScrcpyRuntimeError("plugin scrcpy control socket is not available")
            try:
                connection.sendall(payload)
            except OSError as exc:
                raise ScrcpyRuntimeError(f"scrcpy control send failed: {exc}") from exc

    def _open_scrcpy_sockets(
        self,
        *,
        serial: str,
        port: int,
        process: subprocess.Popen[bytes],
        native_width: int,
        native_height: int,
        max_fps: int,
        resolution_percent: int,
    ) -> tuple[socket.socket, socket.socket, ScrcpyVideoMeta]:
        last_error: Optional[Exception] = None
        for _attempt in range(40):
            video: Optional[socket.socket] = None
            control: Optional[socket.socket] = None
            try:
                if process.poll() is not None:
                    raise ScrcpyRuntimeError(
                        f"scrcpy-server exited before handshake ({process.returncode})"
                    )
                video = socket.create_connection(("127.0.0.1", port), timeout=0.8)
                control = socket.create_connection(("127.0.0.1", port), timeout=0.8)
                dummy = _recv_exact(video, 1)
                if dummy != b"\x00":
                    raise ScrcpyRuntimeError(
                        f"unexpected scrcpy handshake byte: {dummy.hex()}"
                    )
                metadata = _recv_exact(video, DEVICE_NAME_LENGTH + 12)
                raw_name = metadata[:DEVICE_NAME_LENGTH].split(b"\x00", 1)[0]
                device_name = raw_name.decode("utf-8", errors="replace") or serial
                codec_id, width, height = struct.unpack(
                    ">III", metadata[DEVICE_NAME_LENGTH:]
                )
                if codec_id != H264_CODEC_ID:
                    raise ScrcpyRuntimeError(
                        f"scrcpy-server returned unsupported codec 0x{codec_id:08x}"
                    )
                video.settimeout(None)
                control.settimeout(None)
                return (
                    video,
                    control,
                    ScrcpyVideoMeta(
                        serial=serial,
                        device_name=device_name,
                        codec_id=codec_id,
                        width=width,
                        height=height,
                        native_width=native_width,
                        native_height=native_height,
                        max_fps=max_fps,
                        resolution_percent=resolution_percent,
                    ),
                )
            except (OSError, ScrcpyRuntimeError) as exc:
                last_error = exc
                if video:
                    video.close()
                if control:
                    control.close()
                time.sleep(0.2)
        raise ScrcpyRuntimeError(
            f"could not connect to plugin scrcpy-server: {last_error}"
        )

    def _ensure_loopback(self) -> None:
        with self._lock:
            if self._loopback is not None:
                return
            loopback = _ThreadingLoopbackServer(self)
            loopback_thread = threading.Thread(
                target=loopback.serve_forever,
                name="scrcpymac-loopback",
                daemon=True,
            )
            self._loopback = loopback
            self._loopback_thread = loopback_thread
        loopback_thread.start()

    def _pump_video(self, stream_id: int, connection: socket.socket) -> None:
        try:
            while True:
                header = _recv_exact(connection, FRAME_HEADER_LENGTH)
                pts_field, size = struct.unpack(">QI", header)
                payload = _recv_exact(connection, size)
                is_config = bool(pts_field & PACKET_FLAG_CONFIG)
                is_key = bool(pts_field & PACKET_FLAG_KEY_FRAME)
                pts = pts_field & PACKET_PTS_MASK
                flags = (WS_FLAG_CONFIG if is_config else 0) | (
                    WS_FLAG_KEY_FRAME if is_key else 0
                )
                packet = (
                    struct.pack(">BBQI", WS_PACKET_VERSION, flags, pts, len(payload))
                    + payload
                )
                with self._lock:
                    if stream_id != self._stream_id or self._state != "streaming":
                        return
                    if is_config:
                        self._config_packet = packet
                        self._codec = _codec_from_config(payload)
                    else:
                        self._frame_count += 1
                        self._frame_times.append(time.monotonic())
                        if is_key:
                            self._key_packet = packet
                if is_config:
                    self._broadcast_text({"type": "config", "codec": self._codec})
                self._broadcast_binary(packet)
        except (OSError, ScrcpyRuntimeError) as exc:
            with self._lock:
                should_report = stream_id == self._stream_id and self._state == "streaming"
            if should_report:
                self._broadcast_text({"type": "error", "error": str(exc)})
                self._stop_resources(
                    expected_stream_id=stream_id,
                    error=f"H.264 stream ended: {exc}",
                )

    def _broadcast_text(self, payload: dict[str, Any]) -> None:
        for client in self._client_snapshot():
            try:
                client.send_text(payload)
            except OSError:
                self.remove_stream_client(client)
                client.close()

    def _broadcast_binary(self, payload: bytes) -> None:
        for client in self._client_snapshot():
            try:
                client.send_binary(payload)
            except OSError:
                self.remove_stream_client(client)
                client.close()

    def _client_snapshot(self) -> list[_WebSocketClient]:
        with self._lock:
            return list(self._clients)

    def _stop_resources(
        self,
        *,
        expected_stream_id: Optional[int] = None,
        error: str = "",
    ) -> None:
        with self._lock:
            if expected_stream_id is not None and expected_stream_id != self._stream_id:
                return
            self._stream_id += 1
            clients = list(self._clients)
            self._clients.clear()
            video_socket = self._video_socket
            control_socket = self._control_socket
            process = self._server_process
            adb = self._adb
            forward_port = self._forward_port
            video_thread = self._video_thread

            self._video_socket = None
            self._control_socket = None
            self._server_process = None
            self._adb = None
            self._forward_port = 0
            self._video_thread = None
            self._token = ""
            self._config_packet = None
            self._key_packet = None
            self._frame_times.clear()
            if error:
                self._state = "error"
                self._error = error
            else:
                self._state = "idle"
                self._error = ""
                self._meta = None

        for client in clients:
            client.close()
        if video_socket:
            try:
                video_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            video_socket.close()
        if control_socket:
            try:
                control_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            control_socket.close()
        if process:
            self._terminate_process(process)
        if adb and forward_port:
            try:
                adb.run(["forward", "--remove", f"tcp:{forward_port}"], timeout=10)
            except AdbError:
                pass
        if (
            video_thread
            and video_thread is not threading.current_thread()
            and video_thread.is_alive()
        ):
            video_thread.join(timeout=1)

    @staticmethod
    def _terminate_process(process: subprocess.Popen[bytes]) -> None:
        if process.poll() is not None:
            return
        process.terminate()
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=2)

    @staticmethod
    def _drain_process_pipe(pipe: Any) -> None:
        if pipe is None:
            return

        def drain() -> None:
            try:
                while pipe.read(4096):
                    pass
            except OSError:
                pass

        threading.Thread(target=drain, name="scrcpymac-server-log", daemon=True).start()
