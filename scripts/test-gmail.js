#!/usr/bin/env node
/** Quick test: verify Gmail skill can read emails (no LLM needed) */
import { readFileSync, existsSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

// Load .env from project root (parent of scripts/)
const __dir = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(__dir, "..");
const envPath = resolve(projectRoot, ".env");

if (existsSync(envPath)) {
  for (const line of readFileSync(envPath, "utf-8").split("\n")) {
    const m = line.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/);
    if (m) process.env[m[1]] = m[2].replace(/^["']|["']$/g, "").trim();
  }
}

const hasToken = !!(process.env.GMAIL_REFRESH_TOKEN);
const hasClient = !!(process.env.GMAIL_CLIENT_ID ?? process.env.GOOGLE_CLIENT_ID);

const { listEmails, readEmail } = await import("../skills/gmail/index.ts");

async function main() {
  console.log("\n  Testing Gmail skill...\n");
  if (!hasToken) {
    console.error("  No GMAIL_REFRESH_TOKEN in .env");
    console.error(`  .env path: ${envPath}`);
    console.error("  Run 'fathom install gmail' to authenticate.\n");
    process.exit(1);
  }
  if (!hasClient) {
    console.log("  Using built-in OAuth client (add GOOGLE_CLIENT_ID/SECRET to .env if you used your own app)\n");
  }
  try {
    const { messages } = await listEmails({ maxResults: 3 });
    console.log(`  Found ${messages.length} email(s):\n`);
    for (const m of messages) {
      const email = await readEmail({ messageId: m.id });
      console.log(`  • ${email.subject || "(no subject)"}`);
      console.log(`    From: ${email.from}`);
      console.log(`    ${email.snippet?.slice(0, 80) || ""}...\n`);
    }
    console.log("  Gmail skill works!\n");
  } catch (err) {
    console.error("  Error:", err.message);
    if (err.message?.includes("invalid_grant") || err.message?.includes("OAuth")) {
      console.log("\n  Your refresh token may have expired or was issued by a different OAuth app.");
      console.log("  Run 'fathom install gmail' again to re-authenticate.\n");
    } else {
      console.log(`\n  .env path: ${envPath}`);
      console.log("  Ensure GMAIL_REFRESH_TOKEN is set. If you used your own OAuth app, add GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET too.\n");
    }
    process.exit(1);
  }
}

main();
