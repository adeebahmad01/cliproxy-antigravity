#!/usr/bin/env python3
"""Minimal CLIProxyAPI C-ABI and executor smoke test for a built plugin library."""

from __future__ import annotations

import base64
import ctypes
import json
import os
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
  printf 'mock-model-xhigh\\tMock Model (XHigh)\\n'
  exit 0
fi
format="text"
slash=1
conversation=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-format) format="$2"; shift 2 ;;
    --print-timeout) shift 2 ;;
    --conversation) conversation="$2"; shift 2 ;;
    --model|--effort) shift 2 ;;
    --sandbox|--dangerously-skip-permissions) shift ;;
    --disable-slash-commands) slash=0; shift ;;
    -p|--print|--prompt) prompt="$2"; shift 2 ;;
    *) shift ;;
  esac
done
stdin="$(cat)"
if [ "$slash" = "1" ] && [ "$stdin" = "/usage" ]; then
  printf '%s\\n' '{"status":"SUCCESS","response":"","command":{"name":"usage","data":{"groups":[{"name":"Mock Models","description":"Models within this group: Mock Model","buckets":[{"id":"mock-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.5,"reset_time":"2099-01-01T00:00:00Z"}]}]}}}'
  exit 0
fi
if [ "$format" = "json" ]; then
  case "$stdin" in
    *"next turn"*)
      if [ "$conversation" != "mock-conversation" ] || [ "$stdin" != "next turn" ]; then
        printf 'wrong native conversation ID or prompt\\n' >&2
        exit 1
      fi
      ;;
  esac
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
    with tempfile.TemporaryDirectory() as tmp:
        root = pathlib.Path(tmp)
        os.environ.update(HOME=tmp, USERPROFILE=tmp, XDG_CONFIG_HOME=tmp, APPDATA=tmp)
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
        expected_version = os.environ.get("EXPECTED_PLUGIN_VERSION")
        if expected_version is not None:
            actual_version = result["metadata"]["Version"]
            if actual_version != expected_version:
                raise RuntimeError(
                    f"plugin version mismatch: expected {expected_version!r}, got {actual_version!r}"
                )
        assert result["capabilities"]["executor_model_scope"] == "oauth"
        fields = {f["Name"]: f for f in result["metadata"]["ConfigFields"]}
        if fields["reasoning_effort"]["Type"] != "enum" or fields["reasoning_effort"]["EnumValues"] != ["low", "medium", "high", "xhigh", "max"] or fields["binary_path"]["Default"] != "agy":
            raise RuntimeError(f"unexpected config field metadata: {fields}")

        if result["capabilities"].get("model_router"):
            raise RuntimeError("model_router must stay off: models are registered per agy auth")

        rc, model_response = call("model.register", {})
        if rc != 0 or not model_response.get("ok"):
            raise RuntimeError(f"model registration failed: {model_response}")
        model_ids = [m["ID"] for m in model_response["result"]["Models"]]
        if "agy/default" not in model_ids:
            raise RuntimeError(f"expected default model: {model_ids}")

        rc, auth_models = call("model.for_auth", {"Metadata": {"home": tmp}})
        if rc != 0 or not auth_models.get("ok"):
            raise RuntimeError(f"model.for_auth failed: {auth_models}")
        auth_model_by_id = {m["ID"]: m for m in auth_models["result"]["Models"]}
        if "agy/mock-model-high" not in auth_model_by_id or "agy/mock-model-xhigh" not in auth_model_by_id or "agy/mock-model" not in auth_model_by_id:
            raise RuntimeError(f"expected discovered models: {list(auth_model_by_id)}")
        if auth_model_by_id["agy/mock-model-high"]["Name"] != "agy/mock-model-high" or auth_model_by_id["agy/mock-model-high"]["DisplayName"] != "Mock Model (High)":
            raise RuntimeError(f"unexpected Name/DisplayName: {auth_model_by_id['agy/mock-model-high']}")
        model_ids = list(auth_model_by_id)

        rc, route = call("model.route", {"RequestedModel": "agy/mock-model(high)"})
        if rc != 0 or route["result"]["Handled"]:
            raise RuntimeError(f"model.route must not handle under oauth scope: {route}")

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
        conversation_id = openai_payload.get("conversation_id", "")
        if not conversation_id.startswith("agy.v1."):
            raise RuntimeError(f"expected conversation_id in payload: {openai_payload}")

        chat_request["conversation_id"] = conversation_id
        chat_request["messages"] += [
            {"role": "assistant", "content": "mock response"},
            {"role": "user", "content": "next turn"},
        ]
        rc, execution = call(
            "executor.execute",
            {"Model": "agy/mock-model-high", "Payload": base64.b64encode(json.dumps(chat_request).encode()).decode()},
        )
        if rc != 0 or not execution.get("ok"):
            raise RuntimeError(f"resume failed: {execution}")
        resumed = json.loads(base64.b64decode(execution["result"]["Payload"]))
        if resumed.get("conversation_id") != conversation_id:
            raise RuntimeError(f"resume changed the conversation ID: {resumed}")

        # A client prompt must never be expanded as an agy slash command.
        slash_request = {"model": "agy/mock-model-high", "messages": [{"role": "user", "content": "/usage"}]}
        rc, execution = call(
            "executor.execute",
            {"Model": "agy/mock-model-high", "Payload": base64.b64encode(json.dumps(slash_request).encode()).decode()},
        )
        if rc != 0 or not execution.get("ok"):
            raise RuntimeError(f"slash-command executor call failed: {execution}")
        slash_payload = json.loads(base64.b64decode(execution["result"]["Payload"]))
        if slash_payload["choices"][0]["message"]["content"] != "mock response":
            raise RuntimeError(f"client prompt was run as a slash command: {slash_payload}")

        # Client payload faults must not cool down the agy credential.
        bad_request = {"model": "agy/mock-model-high", "messages": []}
        rc, execution = call(
            "executor.execute",
            {"Model": "agy/mock-model-high", "Payload": base64.b64encode(json.dumps(bad_request).encode()).decode()},
        )
        if rc == 0 or execution["error"]["code"] != "request_scoped" or execution["error"]["http_status"] != 400:
            raise RuntimeError(f"expected request_scoped 400: {execution}")

        rc, management = call("management.register", {})
        routes = management.get("result", {}).get("routes", [])
        if rc != 0 or not any(r.get("Path") == "/agy/limits" for r in routes):
            raise RuntimeError(f"expected /agy/limits management route: {management}")
        rc, limits = call("management.handle", {"Method": "GET", "Path": "/v0/management/agy/limits"})
        if rc != 0 or not limits.get("ok") or limits["result"]["StatusCode"] != 200:
            raise RuntimeError(f"limits route failed: {limits}")
        report = json.loads(base64.b64decode(limits["result"]["Body"]))
        if report["groups"][0]["buckets"][0]["remaining_fraction"] != 0.5:
            raise RuntimeError(f"unexpected limits report: {report}")

        rc, quota = call("quota.fetch", {"auth_index": "", "auth_id": "", "provider": "agy"})
        if rc != 0 or not quota.get("ok") or quota["result"]["groups"][0]["displayName"] != "Mock Models":
            raise RuntimeError(f"quota.fetch failed: {quota}")

        api.shutdown()
    print(f"ABI smoke test passed: {result['metadata']['Name']} ({', '.join(model_ids)})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
