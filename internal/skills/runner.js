// Fathom skill subprocess runner.
//
// The Go Fathom binary embeds this file via //go:embed and writes it to
// ~/.cache/fathom/runner.js the first time it spawns a skill. Each skill
// invocation runs as: `node --disallow-code-generation-from-strings
// --no-deprecation --experimental-strip-types runner.js`. We do NOT pass
// --frozen-intrinsics — see sandbox.go for the reasoning (it broke
// Baileys/pino/Sentry-style libs that customise Error.prepareStackTrace,
// and the actual security boundary is subprocess isolation + egress
// proxy + vault-scoped secrets, not Error-prototype immutability).
//
// Wire protocol (one round-trip per process, stdin → stdout JSON):
//   in : {"entryPoint":string,"functionName":string,"input":any,
//         "secrets":Record<string,string>}
//   out: {"ok":true,"result":any}  OR  {"ok":false,"error":string}
//
// The host pre-resolves the skill's declared secrets via the vault (scoped
// to the requesting skill) and ships them in `secrets`. ctx.fetch is
// available iff IRONCLAW_PROXY_URL / FANTAZM_PROXY_URL is set on the env;
// it POSTs to the host's egress proxy so the skill never holds raw tokens.

"use strict";

const { pathToFileURL } = require("node:url");

function readStdin() {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    const MAX = 4 * 1024 * 1024;
    process.stdin.on("data", (chunk) => {
      size += chunk.length;
      if (size > MAX) {
        reject(new Error("Input exceeds 4MB"));
        process.stdin.destroy();
        return;
      }
      chunks.push(chunk);
    });
    process.stdin.on("end", () => resolve(Buffer.concat(chunks).toString("utf-8")));
    process.stdin.on("error", reject);
  });
}

function emit(payload) {
  // Synchronous write to stdout; node will flush on exit. Use the
  // process.stdout._write underlying socket if needed, but for a single
  // JSON object this is fine.
  process.stdout.write(JSON.stringify(payload));
}

async function main() {
  const raw = await readStdin();
  let invocation;
  try {
    invocation = JSON.parse(raw);
  } catch (err) {
    emit({ ok: false, error: "Invalid JSON input: " + err.message });
    process.exit(2);
  }

  const secrets = invocation.secrets || {};
  const proxyUrl = process.env.FANTAZM_PROXY_URL || process.env.IRONCLAW_PROXY_URL;
  const proxyToken = process.env.FANTAZM_PROXY_TOKEN || process.env.IRONCLAW_PROXY_TOKEN;

  const ctx = {
    getSecret(name) {
      if (!(name in secrets)) {
        throw new Error("Secret not provisioned for this invocation: " + name);
      }
      return secrets[name];
    },
    async fetch(url, opts = {}) {
      if (!proxyUrl || !proxyToken) {
        throw new Error("ctx.fetch: egress proxy not configured for this invocation.");
      }
      const res = await fetch(proxyUrl + "/fetch", {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "x-fantazm-token": proxyToken,
        },
        body: JSON.stringify({
          url,
          method: opts.method || "GET",
          headers: opts.headers || {},
          body: opts.body,
          attach: opts.attach || [],
        }),
      });
      if (!res.ok) {
        const txt = await res.text();
        throw new Error("egress proxy error (" + res.status + "): " + txt.slice(0, 200));
      }
      return res.json();
    },
  };

  let mod;
  try {
    mod = await import(pathToFileURL(invocation.entryPoint).href);
  } catch (err) {
    emit({ ok: false, error: "Failed to load skill module: " + err.message });
    process.exit(1);
  }

  const fn = mod[invocation.functionName];
  if (typeof fn !== "function") {
    emit({ ok: false, error: 'Export "' + invocation.functionName + '" is not a function' });
    process.exit(1);
  }

  try {
    const result = await fn(invocation.input, ctx);
    emit({ ok: true, result });
  } catch (err) {
    emit({ ok: false, error: err && err.message ? err.message : String(err) });
    process.exit(1);
  }
}

main().catch((err) => {
  emit({ ok: false, error: err && err.message ? err.message : String(err) });
  process.exit(1);
});
