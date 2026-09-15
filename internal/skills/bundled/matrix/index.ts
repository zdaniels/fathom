// Fathom Matrix skill — Matrix Client-Server API.
//
// Auth: per-user access token (NOT OAuth — Matrix uses long-lived tokens
// you generate by logging in once via the Matrix client of your choice
// or the /login endpoint). Way simpler than Microsoft/Google OAuth.
//
// Setup the user does once:
//   1. Pick a homeserver (matrix.org or self-hosted, doesn't matter)
//   2. Log in via Element / Cinny / whatever and copy the access token
//      from Settings → Help & About → Advanced
//   3. `fathom install matrix` prompts for MATRIX_HOMESERVER (e.g.
//      https://matrix.org), MATRIX_ACCESS_TOKEN, MATRIX_USER_ID (e.g.
//      @alice:matrix.org)
//
// The egress proxy can't inject the token because Matrix uses path-style
// auth (?access_token=) for some endpoints and Bearer for others. We
// pass the token via ctx.getSecret() and the skill builds the headers
// itself. Same trade-off as the telegram skill.

type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

function homeserver(ctx: Ctx): string {
  if (!ctx.getSecret) throw new Error("matrix skill requires ctx.getSecret");
  const hs = ctx.getSecret("MATRIX_HOMESERVER").replace(/\/+$/, "");
  if (!hs.startsWith("https://") && !hs.startsWith("http://")) {
    throw new Error("MATRIX_HOMESERVER must start with https:// (or http:// for local testing)");
  }
  return hs;
}

async function matrixApi(method: string, path: string, body: unknown | undefined, ctx: Ctx): Promise<unknown> {
  if (!ctx.fetch || !ctx.getSecret) throw new Error("matrix skill requires ctx.fetch + ctx.getSecret");
  const token = ctx.getSecret("MATRIX_ACCESS_TOKEN");
  const url = `${homeserver(ctx)}${path}`;
  const res = await ctx.fetch(url, {
    method,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status >= 400) {
    throw new Error(`Matrix API ${res.status}: ${res.body.slice(0, 300)}`);
  }
  return res.body ? JSON.parse(res.body) : null;
}

export async function send_message(
  params: { roomId: string; text: string; html?: string },
  ctx: Ctx,
): Promise<{ eventId: string }> {
  // Use a transaction ID per send to avoid duplicate delivery on retry.
  // Spec recommends a unique token per send; using Date.now() is fine for
  // a single-user agent.
  const txnId = `fathom-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
  const body: Record<string, unknown> = {
    msgtype: "m.text",
    body: params.text,
  };
  if (params.html) {
    body.format = "org.matrix.custom.html";
    body.formatted_body = params.html;
  }
  const path = `/_matrix/client/v3/rooms/${encodeURIComponent(params.roomId)}/send/m.room.message/${txnId}`;
  const result = (await matrixApi("PUT", path, body, ctx)) as { event_id: string };
  return { eventId: result.event_id };
}

export async function list_rooms(
  _params: Record<string, never> | undefined,
  ctx: Ctx,
): Promise<{ rooms: Array<{ id: string; name: string; topic: string; memberCount: number }> }> {
  // Matrix's joined_rooms gives just IDs; getting names requires a per-room
  // state lookup. Fetch /sync once with timeout=0 — gets every joined room
  // with metadata in one shot.
  const sync = (await matrixApi("GET", "/_matrix/client/v3/sync?timeout=0", undefined, ctx)) as {
    rooms?: {
      join?: Record<string, {
        state?: { events?: Array<{ type: string; content?: { name?: string; topic?: string }; state_key?: string }> };
        timeline?: { events?: Array<{ type: string; content?: { membership?: string } }> };
      }>;
    };
  };
  const rooms = sync.rooms?.join ?? {};
  return {
    rooms: Object.entries(rooms).map(([id, r]) => {
      let name = id;
      let topic = "";
      let memberCount = 0;
      for (const ev of r.state?.events ?? []) {
        if (ev.type === "m.room.name" && ev.content?.name) name = ev.content.name;
        else if (ev.type === "m.room.topic" && ev.content?.topic) topic = ev.content.topic;
        else if (ev.type === "m.room.member" && ev.content?.membership === "join") memberCount++;
      }
      return { id, name, topic, memberCount };
    }),
  };
}

export async function list_room_messages(
  params: { roomId: string; limit?: number },
  ctx: Ctx,
): Promise<{ messages: Array<{ id: string; sender: string; text: string; timestamp: number }> }> {
  const limit = Math.min(Math.max(params.limit ?? 30, 1), 200);
  const path = `/_matrix/client/v3/rooms/${encodeURIComponent(params.roomId)}/messages?dir=b&limit=${limit}`;
  const result = (await matrixApi("GET", path, undefined, ctx)) as {
    chunk?: Array<{ event_id: string; sender: string; origin_server_ts: number; type: string; content?: { body?: string } }>;
  };
  const messages = (result.chunk ?? [])
    .filter((e) => e.type === "m.room.message" && e.content?.body)
    .map((e) => ({
      id: e.event_id,
      sender: e.sender,
      text: e.content?.body ?? "",
      timestamp: e.origin_server_ts,
    }))
    .reverse(); // /messages?dir=b returns newest-first; flip so the agent reads chronologically
  return { messages };
}

export async function whoami(
  _params: Record<string, never> | undefined,
  ctx: Ctx,
): Promise<{ userId: string; deviceId: string }> {
  const result = (await matrixApi("GET", "/_matrix/client/v3/account/whoami", undefined, ctx)) as {
    user_id: string;
    device_id?: string;
  };
  return { userId: result.user_id, deviceId: result.device_id ?? "" };
}

export const tools = [
  { name: "send_message", description: "Send a text message to a Matrix room. Pass roomId like !abc123:matrix.org. Optional `html` for formatted_body.", parameters: { type: "object", properties: { roomId: { type: "string" }, text: { type: "string" }, html: { type: "string" } }, required: ["roomId", "text"] } },
  { name: "list_rooms", description: "List rooms the authenticated user has joined, with name + topic + member count.", parameters: { type: "object", properties: {}, required: [] } },
  { name: "list_room_messages", description: "Read recent messages from a room, newest at the end.", parameters: { type: "object", properties: { roomId: { type: "string" }, limit: { type: "number", description: "Default 30, max 200" } }, required: ["roomId"] } },
  { name: "whoami", description: "Verify the configured access token and report the matrix user id + device id.", parameters: { type: "object", properties: {}, required: [] } },
];
