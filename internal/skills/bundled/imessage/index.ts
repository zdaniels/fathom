// Fathom iMessage skill — macOS-only via AppleScript + chat.db.
//
// Sending uses `osascript` to drive Messages.app. Reading uses sqlite3
// against ~/Library/Messages/chat.db (read-only).
//
// PREREQUISITE — the user must grant Full Disk Access to the terminal
// they run Fathom from (System Settings → Privacy & Security → Full
// Disk Access → enable iTerm/Terminal/whatever). Without it the chat.db
// read fails with EPERM. Sending doesn't need it; only reading does.
//
// Why a skill not a builtin: shells out to osascript + sqlite3 which
// are macOS-specific, and policy gating wants this clearly separated
// from the general `bash` tool.

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { homedir, platform } from "node:os";
import { join } from "node:path";

const execFileP = promisify(execFile);

type Ctx = {
  getSecret?: (name: string) => string;
};

function ensureMacOS(): void {
  if (platform() !== "darwin") {
    throw new Error("imessage skill is macOS-only");
  }
}

// AppleScript injection guard: only allow alphanumerics + safe punctuation
// in the `to` field. Everything else gets rejected. The text body is
// quoted via JSON.stringify so embedded quotes are fine.
function safeRecipient(to: string): string {
  if (!/^[A-Za-z0-9 .,_@+\-]{1,200}$/.test(to)) {
    throw new Error("invalid recipient — must be phone, email, or contact name");
  }
  return to;
}

export async function send_message(
  params: { to: string; text: string },
  _ctx: Ctx,
): Promise<{ ok: true }> {
  ensureMacOS();
  const to = safeRecipient(params.to);
  const text = params.text;
  // Build an AppleScript that:
  //   1. Locates the iMessage service.
  //   2. Resolves `to` as a buddy (matches phone, email, or display name).
  //   3. Sends text.
  // `text` is wrapped as an AppleScript string literal — escape backslashes
  // and quotes only; AppleScript doesn't have richer escape syntax.
  const asText = text.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  const script = `
tell application "Messages"
  set targetService to 1st service whose service type = iMessage
  set targetBuddy to buddy "${to}" of targetService
  send "${asText}" to targetBuddy
end tell`;
  await execFileP("osascript", ["-e", script], { timeout: 15000 });
  return { ok: true };
}

export async function list_recent_messages(
  params: { limit?: number; chat?: string } | undefined,
  _ctx: Ctx,
): Promise<{ messages: Array<{ from: string; chat: string; text: string; timestamp: string; isFromMe: boolean }> }> {
  ensureMacOS();
  const limit = Math.min(Math.max(params?.limit ?? 20, 1), 200);
  // chat.db's date column is in nanoseconds since 2001-01-01 (Apple epoch).
  // Convert to ISO 8601 in SQL.
  const where = params?.chat ? `WHERE chat.display_name LIKE '%' || ? || '%' OR chat.chat_identifier LIKE '%' || ? || '%'` : "";
  const args = params?.chat ? [params.chat, params.chat] : [];
  const query = `
SELECT
  COALESCE(handle.id, 'me') AS from_handle,
  COALESCE(chat.display_name, chat.chat_identifier, 'unknown') AS chat_name,
  COALESCE(message.text, '') AS body,
  datetime(message.date / 1000000000 + 978307200, 'unixepoch') AS ts,
  message.is_from_me AS from_me
FROM message
LEFT JOIN handle ON message.handle_id = handle.ROWID
LEFT JOIN chat_message_join ON message.ROWID = chat_message_join.message_id
LEFT JOIN chat ON chat_message_join.chat_id = chat.ROWID
${where}
ORDER BY message.date DESC
LIMIT ${limit};`.trim();
  const dbPath = join(homedir(), "Library", "Messages", "chat.db");
  const { stdout } = await execFileP(
    "sqlite3",
    ["-readonly", "-separator", "", dbPath, query, ...args],
    { timeout: 10000, maxBuffer: 4 * 1024 * 1024 },
  ).catch((err: { message?: string; code?: string }) => {
    if (err.message?.includes("authorization denied") || err.message?.includes("unable to open")) {
      throw new Error(
        "iMessage read denied: grant Full Disk Access to your terminal in System Settings → Privacy & Security → Full Disk Access.",
      );
    }
    throw err;
  });
  const messages = stdout
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => {
      const [from, chat, text, ts, fromMe] = line.split("");
      return {
        from: from ?? "unknown",
        chat: chat ?? "unknown",
        text: text ?? "",
        timestamp: ts ?? "",
        isFromMe: fromMe === "1",
      };
    });
  return { messages };
}

export async function list_chats(
  params: { limit?: number } | undefined,
  _ctx: Ctx,
): Promise<{ chats: Array<{ id: string; name: string; lastMessage: string; lastSeen: string }> }> {
  ensureMacOS();
  const limit = Math.min(Math.max(params?.limit ?? 30, 1), 200);
  const query = `
SELECT
  chat.chat_identifier,
  COALESCE(chat.display_name, chat.chat_identifier, 'unknown') AS name,
  COALESCE(last.text, '') AS last_text,
  datetime(MAX(message.date) / 1000000000 + 978307200, 'unixepoch') AS last_seen
FROM chat
JOIN chat_message_join cmj ON chat.ROWID = cmj.chat_id
JOIN message ON cmj.message_id = message.ROWID
LEFT JOIN message last ON last.ROWID = (
  SELECT message_id FROM chat_message_join WHERE chat_id = chat.ROWID
  ORDER BY message_id DESC LIMIT 1
)
GROUP BY chat.ROWID
ORDER BY MAX(message.date) DESC
LIMIT ${limit};`.trim();
  const dbPath = join(homedir(), "Library", "Messages", "chat.db");
  const { stdout } = await execFileP(
    "sqlite3",
    ["-readonly", "-separator", "", dbPath, query],
    { timeout: 10000, maxBuffer: 4 * 1024 * 1024 },
  ).catch((err: { message?: string }) => {
    if (err.message?.includes("authorization denied") || err.message?.includes("unable to open")) {
      throw new Error(
        "iMessage read denied: grant Full Disk Access to your terminal in System Settings → Privacy & Security → Full Disk Access.",
      );
    }
    throw err;
  });
  const chats = stdout
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => {
      const [id, name, lastText, lastSeen] = line.split("");
      return {
        id: id ?? "",
        name: name ?? "unknown",
        lastMessage: lastText ?? "",
        lastSeen: lastSeen ?? "",
      };
    });
  return { chats };
}

export const tools = [
  { name: "send_message", description: "Send an iMessage via Messages.app. 'to' is a phone number, email, or contact display name that already exists in your iMessage contacts. macOS only.", parameters: { type: "object", properties: { to: { type: "string" }, text: { type: "string" } }, required: ["to", "text"] } },
  { name: "list_recent_messages", description: "Read recent iMessages from the local Messages database. Requires Full Disk Access for your terminal. Optionally filter by chat name or identifier.", parameters: { type: "object", properties: { limit: { type: "number", description: "Default 20, max 200" }, chat: { type: "string", description: "Optional substring of chat name/identifier" } }, required: [] } },
  { name: "list_chats", description: "List recent iMessage conversations, sorted by most-recent activity. Requires Full Disk Access.", parameters: { type: "object", properties: { limit: { type: "number", description: "Default 30, max 200" } }, required: [] } },
];
