#!/usr/bin/env python3
"""Fathom skill subprocess runner (Python).

The Go Fathom binary embeds this file via //go:embed and writes it to
~/.cache/fathom/runner.py the first time it spawns a Python skill.
Each skill invocation runs as: `python3 runner.py` (or whichever
interpreter the manifest's language hint points at).

Wire protocol — same as runner.js, one round-trip per process:

    in (stdin) : {
        "entryPoint":   "/abs/path/to/skill.py",
        "functionName": "search",
        "input":        { ... },
        "secrets":      { "FOO_API_KEY": "..." }
    }
    out (stdout): {"ok": true,  "result": any}
              OR {"ok": false, "error": "..."}

The host pre-resolves the skill's declared secrets via the vault
(scoped to the requesting skill) and ships them in `secrets`. We
expose them through `ctx.get_secret(name)`. ctx.fetch is available iff
FANTAZM_PROXY_URL is set on the env; it POSTs to the egress proxy so
the skill never holds raw tokens.

Skill functions may be sync OR async (`async def`). We detect with
inspect.iscoroutinefunction and `asyncio.run` the result if needed.
"""

from __future__ import annotations

import asyncio
import importlib.util
import inspect
import json
import os
import sys
import traceback
import urllib.error
import urllib.request
from typing import Any


# --- single-file logging ------------------------------------------------
# Errors only go to stderr (host logs them); stdout is the JSON response
# channel and MUST stay clean.

def _emit(payload: dict[str, Any]) -> None:
    sys.stdout.write(json.dumps(payload))
    sys.stdout.flush()


def _err(payload: dict[str, Any], exit_code: int = 1) -> None:
    _emit(payload)
    sys.exit(exit_code)


# --- ctx ----------------------------------------------------------------

class Ctx:
    """Runtime context for skill code. Matches the TS `Ctx` shape:

        ctx.get_secret(name) -> str
        await ctx.fetch(url, method=..., headers=..., body=..., attach=...)

    Both methods raise informative RuntimeErrors when not provisioned
    (secret not declared in manifest / proxy not configured).
    """

    def __init__(self, secrets: dict[str, str], proxy_url: str | None, proxy_token: str | None):
        self._secrets = secrets
        self._proxy_url = proxy_url
        self._proxy_token = proxy_token

    def get_secret(self, name: str) -> str:
        if name not in self._secrets:
            raise RuntimeError(f"Secret not provisioned for this invocation: {name}")
        return self._secrets[name]

    async def fetch(
        self,
        url: str,
        *,
        method: str = "GET",
        headers: dict[str, str] | None = None,
        body: str | None = None,
        attach: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        if not self._proxy_url or not self._proxy_token:
            raise RuntimeError("ctx.fetch: egress proxy not configured for this invocation.")

        payload = json.dumps(
            {
                "url": url,
                "method": method,
                "headers": headers or {},
                "body": body,
                "attach": attach or [],
            }
        ).encode("utf-8")

        req = urllib.request.Request(
            self._proxy_url.rstrip("/") + "/fetch",
            data=payload,
            method="POST",
            headers={
                "content-type": "application/json",
                "x-fantazm-token": self._proxy_token,
            },
        )

        # The egress proxy is synchronous HTTP. We run it in a thread so
        # the skill's `await ctx.fetch(...)` doesn't block the event loop
        # — even though the runner itself is single-shot, async libs
        # composed downstream might depend on the loop staying live.
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(None, self._do_proxy_request, req)

    @staticmethod
    def _do_proxy_request(req: urllib.request.Request) -> dict[str, Any]:
        try:
            with urllib.request.urlopen(req, timeout=300) as resp:
                body = resp.read().decode("utf-8")
                return json.loads(body)
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", errors="replace")[:200]
            raise RuntimeError(f"egress proxy error ({e.code}): {body}") from e


# --- entry --------------------------------------------------------------

def _load_module(entry_point: str) -> Any:
    """Import a skill module from an absolute filesystem path.

    The host always passes an absolute path. Module name is derived
    from the file basename so multiple sibling `index.py` files don't
    collide in sys.modules across invocations (they each run in a
    fresh subprocess anyway, but be explicit).
    """
    base = os.path.basename(entry_point)
    mod_name = "_fathom_skill_" + os.path.splitext(base)[0]
    spec = importlib.util.spec_from_file_location(mod_name, entry_point)
    if spec is None or spec.loader is None:
        raise ImportError(f"Could not build module spec for {entry_point}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[mod_name] = module
    spec.loader.exec_module(module)
    return module


async def _invoke(module: Any, fn_name: str, input_: Any, ctx: Ctx) -> Any:
    fn = getattr(module, fn_name, None)
    if fn is None or not callable(fn):
        raise RuntimeError(f'Export "{fn_name}" is not a function')
    if inspect.iscoroutinefunction(fn):
        return await fn(input_, ctx)
    return fn(input_, ctx)


def main() -> None:
    raw = sys.stdin.read(4 * 1024 * 1024 + 1)
    if len(raw) > 4 * 1024 * 1024:
        _err({"ok": False, "error": "Input exceeds 4MB"}, exit_code=2)

    try:
        invocation = json.loads(raw)
    except json.JSONDecodeError as e:
        _err({"ok": False, "error": f"Invalid JSON input: {e}"}, exit_code=2)
        return

    secrets: dict[str, str] = invocation.get("secrets") or {}
    proxy_url = os.environ.get("FANTAZM_PROXY_URL") or os.environ.get("IRONCLAW_PROXY_URL")
    proxy_token = os.environ.get("FANTAZM_PROXY_TOKEN") or os.environ.get("IRONCLAW_PROXY_TOKEN")

    ctx = Ctx(secrets, proxy_url, proxy_token)

    try:
        module = _load_module(invocation["entryPoint"])
    except Exception as e:
        _err({"ok": False, "error": f"Failed to load skill module: {e}"})
        return

    try:
        result = asyncio.run(
            _invoke(module, invocation["functionName"], invocation.get("input"), ctx)
        )
        _emit({"ok": True, "result": result})
    except Exception as e:
        # Include only the top-level message — full traceback goes to
        # stderr where the host's slog picks it up.
        sys.stderr.write(traceback.format_exc())
        _err({"ok": False, "error": str(e)})


if __name__ == "__main__":
    main()
