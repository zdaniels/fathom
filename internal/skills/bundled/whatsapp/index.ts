// Fathom WhatsApp skill — real Baileys integration.
//
// WhatsApp doesn't expose a real bot API; Baileys connects to WhatsApp
// Web by impersonating a paired device. On first run we generate a QR
// code; the user scans it with WhatsApp on their phone (Settings → Linked
// Devices → Link a device) and we receive an auth state that gets
// persisted to FANTAZM_DATA_DIR/whatsapp-auth so subsequent runs reconnect
// silently.
//
// Pairing is normally driven by `fathom install whatsapp` which spawns
// this skill in pair mode and prints the QR to the terminal.

import {
  default as makeWASocket,
  DisconnectReason,
  useMultiFileAuthState,
  type WASocket,
} from "@whiskeysockets/baileys";
import qrcode from "qrcode-terminal";
import pino from "pino";
import { join } from "node:path";

const AUTH_DIR_NAME = "whatsapp-auth";

type Ctx = {
  getSecret?: (name: string) => string;
};

let _sock: WASocket | null = null;
let _connectPromise: Promise<WASocket> | null = null;

function authDir(): string {
  const base = process.env.FANTAZM_DATA_DIR || join(process.env.HOME ?? ".", ".fantazm");
  return join(base, AUTH_DIR_NAME);
}

// connectAndStabilize opens a Baileys socket and waits for it to be
// genuinely ready to send messages. WhatsApp's returning-device flow
// emits the same 515 "restart required" close that we see during the
// initial pair: connection.open fires, then ~immediately the socket
// closes with code 515, then a fresh reconnect-with-saved-state stays
// open. Sending a message in the brief first-open window fails with
// "Connection Closed" mid-flight.
//
// The stabilize loop: open → wait briefly to see if a 515 close fires
// → if so, reconnect; if no close inside the grace window we assume
// the socket is good. Cap at 3 reconnects (matches the install-time
// pair driver's heuristic).
async function connectAndStabilize(opts: { printQR?: boolean } = {}): Promise<WASocket> {
  for (let attempt = 0; attempt < 4; attempt++) {
    const { state, saveCreds } = await useMultiFileAuthState(authDir());
    const sock = makeWASocket({
      auth: state,
      printQRInTerminal: false,
      logger: pino({ level: "silent" }) as never,
    });
    sock.ev.on("creds.update", saveCreds);
    if (opts.printQR) {
      sock.ev.on("connection.update", (update) => {
        if (update.qr) {
          qrcode.generate(update.qr, { small: true }, (rendered) => {
            process.stderr.write("\n" + rendered + "\n");
            process.stderr.write("Scan with WhatsApp → Settings → Linked Devices → Link a device\n\n");
          });
        }
      });
    }
    // Wait for the first terminal state (open, close, or 12s timeout).
    const result = await new Promise<{ kind: "open" | "close" | "timeout"; code?: number }>((resolve) => {
      const timer = setTimeout(() => resolve({ kind: "timeout" }), 12_000);
      const handler = (update: { connection?: string; lastDisconnect?: { error?: { output?: { statusCode?: number } } } }) => {
        if (update.connection === "open") {
          clearTimeout(timer);
          sock.ev.off("connection.update", handler);
          resolve({ kind: "open" });
        } else if (update.connection === "close") {
          clearTimeout(timer);
          sock.ev.off("connection.update", handler);
          resolve({ kind: "close", code: update.lastDisconnect?.error?.output?.statusCode });
        }
      };
      sock.ev.on("connection.update", handler);
    });
    if (result.kind === "open") {
      // Connection is open. Watch for a 515 close in the next 2 seconds —
      // if it fires we loop and create a fresh socket; otherwise the
      // session is stable and we return the live socket.
      const closeAfterOpen = await new Promise<{ code?: number } | null>((resolve) => {
        const timer = setTimeout(() => resolve(null), 2_000);
        const handler = (update: { connection?: string; lastDisconnect?: { error?: { output?: { statusCode?: number } } } }) => {
          if (update.connection === "close") {
            clearTimeout(timer);
            sock.ev.off("connection.update", handler);
            resolve({ code: update.lastDisconnect?.error?.output?.statusCode });
          }
        };
        sock.ev.on("connection.update", handler);
      });
      if (closeAfterOpen === null) {
        return sock; // stable
      }
      // Fell through: connection closed shortly after open. Loop to reconnect.
      continue;
    }
    if (result.kind === "close") {
      const code = result.code ?? 0;
      if (code === DisconnectReason.loggedOut) {
        throw new Error("WhatsApp session is logged out — re-run `fathom install whatsapp` to re-pair");
      }
      // Otherwise (515, transient) loop and reconnect.
      continue;
    }
    // timeout — try again
  }
  throw new Error("WhatsApp connection did not stabilize after 4 attempts");
}

async function connect(opts: { printQR?: boolean } = {}): Promise<WASocket> {
  if (_sock) return _sock;
  if (_connectPromise) return _connectPromise;
  _connectPromise = (async () => {
    const sock = await connectAndStabilize(opts);
    sock.ev.on("connection.update", (update) => {
      if (update.connection === "close") {
        const code = (update.lastDisconnect?.error as { output?: { statusCode?: number } })?.output?.statusCode;
        if (code !== DisconnectReason.loggedOut) {
          _sock = null;
          _connectPromise = null;
        }
      }
    });
    _sock = sock;
    return sock;
  })();
  return _connectPromise;
}

export async function send_message(
  params: { to: string; text: string },
  _ctx: Ctx,
): Promise<{ messageId: string }> {
  const sock = await connect();
  const jid = normalizeJid(params.to);
  const result = await sock.sendMessage(jid, { text: params.text });
  return { messageId: result?.key.id ?? "" };
}

export async function list_chats(
  params: { limit?: number } | undefined,
  _ctx: Ctx,
): Promise<{ chats: Array<{ id: string; name: string; lastMessage?: string }> }> {
  const sock = await connect();
  await awaitOnline(sock);
  const limit = params?.limit ?? 20;
  const store = (sock as unknown as { store?: { chats?: { all?: () => Array<{ id: string; name?: string; lastMessage?: { text?: string } }> } } }).store;
  const chats = store?.chats?.all?.() ?? [];
  return {
    chats: chats.slice(0, limit).map((c) => ({
      id: c.id,
      name: c.name ?? c.id,
      lastMessage: c.lastMessage?.text,
    })),
  };
}

export async function get_me(
  _params: Record<string, never> | undefined,
  _ctx: Ctx,
): Promise<{ id: string; name: string }> {
  const sock = await connect();
  await awaitOnline(sock);
  return {
    id: sock.user?.id ?? "",
    name: sock.user?.name ?? "",
  };
}

export async function pair(
  _params: Record<string, never> | undefined,
  _ctx: Ctx,
): Promise<{ ok: true; user: string }> {
  const sock = await connect({ printQR: true });
  await awaitOnline(sock);
  return { ok: true, user: sock.user?.name ?? sock.user?.id ?? "paired" };
}

function normalizeJid(input: string): string {
  if (input.includes("@")) return input;
  const digits = input.replace(/[^0-9]/g, "");
  return `${digits}@s.whatsapp.net`;
}

export const tools = [
  { name: "send_message", description: "Send a WhatsApp message. 'to' may be a phone number (international format, digits only) or a full JID like 14155551234@s.whatsapp.net.", parameters: { type: "object", properties: { to: { type: "string" }, text: { type: "string" } }, required: ["to", "text"] } },
  { name: "list_chats", description: "List recent WhatsApp chats with their id, name, and last message preview.", parameters: { type: "object", properties: { limit: { type: "number" } }, required: [] } },
  { name: "get_me", description: "Get info about the paired WhatsApp account.", parameters: { type: "object", properties: {}, required: [] } },
  { name: "pair", description: "Pair-mode entrypoint — prints a QR to scan. Normally called by `fathom install whatsapp`.", parameters: { type: "object", properties: {}, required: [] } },
];
