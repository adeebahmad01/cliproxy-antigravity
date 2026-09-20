#!/usr/bin/env python3
"""Minimal CLIProxyAPI C-ABI and executor smoke test for a built plugin library."""

from __future__ import annotations

import base64
import ctypes
import json
import pathlib
import stat
import sys
import tempfile


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


Call = ctypes.CFUNCTYPE(
    ctypes.c_int,
    ctypes.c_char_p,
    ctypes.POINTER(ctypes.c_uint8),
    ctypes.c_size_t,
    ctypes.POINTER(Buffer),
)
Free = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
Shutdown = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("call", Call),
        ("free_buffer", Free),
        ("shutdown", Shutdown),
    ]


def make_mock_agy(root: pathlib.Path) -> pathlib.Path:
    path = root / "agy"
    path.write_text(
        """#!/bin/sh
if [ "$1" = "models" ]; then
  printf 'mock-model-high\\tMock Model (High)\\n'
  exit 0
fi
format="text"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-format) format="$2"; shift 2 ;;
    --print-timeout) shift 2 ;;
    --model|--conversation|--effort) shift 2 ;;
    --sandbox|--dangerously-skip-permissions) shift ;;
    -p|--print|--prompt) prompt="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ "$format" = "json" ]; then
  printf '%s\\n' '{"conversation_id":"mock-conversation","status":"SUCCESS","response":"mock response","usage":{"input_tokens":10,"output_tokens":3,"thinking_tokens":1,"cache_read_tokens":2,"total_tokens":13}}'
  exit 0
fi
printf 'mock response\\n'
"""
    )
    path.chmod(path.stat().st_mode | stat.S_IXUSR)
    return path


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} /path/to/plugin.so", file=sys.stderr)
        return 2

    path = pathlib.Path(sys.argv[1]).resolve()
    lib = ctypes.CDLL(str(path))
    lib.cliproxy_plugin_init.argtypes = [ctypes.c_void_p, ctypes.POINTER(PluginAPI)]
    lib.cliproxy_plugin_init.restype = ctypes.c_int

    api = PluginAPI()
    rc = lib.cliproxy_plugin_init(None, ctypes.byref(api))
    if rc != 0 or api.abi_version != 1:
        raise RuntimeError(f"plugin init failed: rc={rc}, abi={api.abi_version}")

    def call(method: str, payload: dict) -> tuple[int, dict]:
        raw = json.dumps(payload).encode()
        array = (ctypes.c_uint8 * len(raw)).from_buffer_copy(raw) if raw else None
        out = Buffer()
        call_rc = api.call(method.encode(), array, len(raw), ctypes.byref(out))
        data = ctypes.string_at(out.ptr, out.len) if out.ptr else b""
        if out.ptr:
            api.free_buffer(out.ptr, out.len)
        return call_rc, json.loads(data)

    with tempfile.TemporaryDirectory() as tmp:
        root = pathlib.Path(tmp)
        mock_agy = make_mock_agy(root)
        config = f'binary_path: "{mock_agy}"\nprint_timeout: 30m\n'.encode()
        rc, registration = call(
            "plugin.register",
            {
                "config_yaml": base64.b64encode(config).decode(),
                "schema_version": 6,
            },
        )
        if rc != 0 or not registration.get("ok"):
            raise RuntimeError(f"registration failed: {registration}")
        result = registration["result"]
        assert result["capabilities"]["executor_model_scope"] == "static"

        rc, model_response = call("model.register", {})
        if rc != 0 or not model_response.get("ok"):
            raise RuntimeError(f"model registration failed: {model_response}")
        model_ids = [m["ID"] for m in model_response["result"]["Models"]]
        if "agy/default" not in model_ids or "agy/mock-model-high" not in model_ids:
            raise RuntimeError(f"expected default + discovered model: {model_ids}")

        chat_request = {
            "model": "agy/mock-model-high",
            "messages": [{"role": "user", "content": "hello"}],
            "reasoning_effort": "high",
        }
        rc, execution = call(
            "executor.execute",
            {
                "Model": "agy/mock-model-high",
                "Payload": base64.b64encode(json.dumps(chat_request).encode()).decode(),
            },
        )
        if rc != 0 or not execution.get("ok"):
            raise RuntimeError(f"executor failed: {execution}")
        openai_payload = json.loads(base64.b64decode(execution["result"]["Payload"]))
        content = openai_payload["choices"][0]["message"]["content"]
        if content != "mock response":
            raise RuntimeError(f"unexpected assistant content: {content!r}")
        if openai_payload["usage"]["total_tokens"] != 13:
            raise RuntimeError(f"unexpected usage: {openai_payload['usage']}")
        if openai_payload.get("conversation_id") != "mock-conversation":
            raise RuntimeError(f"expected conversation_id in payload: {openai_payload}")

    api.shutdown()
    print(f"ABI smoke test passed: {result['metadata']['Name']} ({', '.join(model_ids)})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
