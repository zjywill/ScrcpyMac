import os
import socket
import struct
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from phone_agent.scrcpy_runtime import (
    ScrcpyRuntime,
    ScrcpyVideoMeta,
    _codec_from_config,
    _ws_frame,
    resolve_scrcpy_server_path,
)


class ScrcpyRuntimeTests(unittest.TestCase):
    def test_resolves_plugin_bundled_scrcpy_server(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            server = root / "share" / "scrcpy-server"
            server.parent.mkdir(parents=True)
            server.write_bytes(b"server")

            with patch.dict(
                os.environ,
                {"PHONE_AGENT_ROOT": str(root), "SCRCPY_SERVER_PATH": ""},
                clear=False,
            ):
                self.assertEqual(resolve_scrcpy_server_path(), str(server))

    def test_prefers_target_layout_over_legacy_scrcpy_server(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            target = root / "share" / "scrcpy-server"
            legacy = root / "bin" / "darwin" / "share" / "scrcpy-server"
            target.parent.mkdir(parents=True)
            legacy.parent.mkdir(parents=True)
            target.write_bytes(b"target")
            legacy.write_bytes(b"legacy")

            with patch.dict(
                os.environ,
                {"PHONE_AGENT_ROOT": str(root), "SCRCPY_SERVER_PATH": ""},
                clear=False,
            ):
                self.assertEqual(resolve_scrcpy_server_path(), str(target))

    def test_derives_webcodecs_codec_string_from_sps(self) -> None:
        config = (
            b"\x00\x00\x00\x01\x67\x64\x00\x29\xaa"
            b"\x00\x00\x00\x01\x68\xee\x3c\x80"
        )
        self.assertEqual(_codec_from_config(config), "avc1.640029")

    def test_websocket_binary_frame_uses_extended_length(self) -> None:
        frame = _ws_frame(2, b"x" * 512)
        self.assertEqual(frame[0], 0x82)
        self.assertEqual(frame[1], 126)
        self.assertEqual(struct.unpack(">H", frame[2:4])[0], 512)
        self.assertEqual(frame[4:], b"x" * 512)

    def test_runtime_touch_uses_scrcpy_control_socket(self) -> None:
        runtime = ScrcpyRuntime()
        sender, receiver = socket.socketpair()
        try:
            runtime._state = "streaming"
            runtime._control_socket = sender
            runtime._meta = ScrcpyVideoMeta(
                serial="device-1",
                device_name="Test",
                codec_id=0x68323634,
                width=540,
                height=1140,
                native_width=1080,
                native_height=2280,
                max_fps=60,
                resolution_percent=50,
            )

            result = runtime.tap_relative(0.5, 0.25)
            wire = receiver.recv(64)

            self.assertEqual(result["backend"], "plugin-control")
            self.assertEqual(len(wire), 64)
            self.assertEqual(wire[0], 2)
            self.assertEqual(wire[1], 0)
            self.assertEqual(wire[32], 2)
            self.assertEqual(wire[33], 1)
        finally:
            runtime._control_socket = None
            sender.close()
            receiver.close()
            runtime.close()

    def test_status_never_exposes_stream_without_token_and_port(self) -> None:
        runtime = ScrcpyRuntime()
        runtime._state = "streaming"
        runtime._meta = ScrcpyVideoMeta(
            serial="device-1",
            device_name="Test",
            codec_id=0x68323634,
            width=540,
            height=1140,
            native_width=1080,
            native_height=2280,
            max_fps=60,
            resolution_percent=50,
        )

        status = runtime.status()

        self.assertEqual(status["backend"], "plugin-h264")
        self.assertNotIn("streamUrl", status)
        runtime.close()

    def test_stream_domains_include_exact_persistent_loopback_origin(self) -> None:
        runtime = ScrcpyRuntime()
        try:
            domains = runtime.stream_connect_domains()
            port = runtime._loopback.server_address[1]

            self.assertIn(f"http://127.0.0.1:{port}", domains)
            self.assertIn(f"ws://127.0.0.1:{port}", domains)
            runtime.stop()
            self.assertEqual(runtime._loopback.server_address[1], port)
        finally:
            runtime.close()

    def test_loopback_bind_failure_leaves_clean_error_state(self) -> None:
        runtime = ScrcpyRuntime()
        with (
            patch("phone_agent.scrcpy_runtime.AdbClient"),
            patch(
                "phone_agent.scrcpy_runtime.resolve_scrcpy_server_path",
                return_value="/tmp/scrcpy-server",
            ),
            patch(
                "phone_agent.scrcpy_runtime._ThreadingLoopbackServer",
                side_effect=PermissionError("bind denied"),
            ),
        ):
            with self.assertRaises(PermissionError):
                runtime.start("device-1")

        status = runtime.status()
        self.assertEqual(status["state"], "error")
        self.assertIn("bind denied", status["error"])
        self.assertNotIn("streamUrl", status)
        runtime.close()


if __name__ == "__main__":
    unittest.main()
