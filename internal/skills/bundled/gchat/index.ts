// Fathom Google Chat skill — Workspace chat via the Chat REST API.
//
// Auth: same Google OAuth pipeline as gmail/calendar. Stores
// GCHAT_CLIENT_ID + GCHAT_CLIENT_SECRET + GCHAT_REFRESH_TOKEN; the
// egress proxy exchanges the refresh token for an access token per call.
//
// Required Google API: enable "Google Chat API" + (optionally) "Chat
// API" scopes in your GCP project. The OAuth client must be type
// "Desktop app" with no preset redirect URI restriction.

const BASE = "https://chat.googleapis.com/v1";

type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const GCHAT_OAUTH_ATTACH = {
  type: "google-oauth" as const,
  refreshTokenSecret: "GCHAT_REFRESH_TOKEN",
  clientIdSecret: "GCHAT_CLIENT_ID",
  clientSecretSecret: "GCHAT_CLIENT_SECRET",
};

async function chatApi(method: string, path: string, body: unknown | undefined, ctx: Ctx): Promise<unknown> {
  if (!ctx.fetch) throw new Error("gchat skill requires ctx.fetch");
  const res = await ctx.fetch(`${BASE}${path}`, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
    attach: [GCHAT_OAUTH_ATTACH],
  });
  if (res.status >= 400) throw new Error(`Chat API ${res.status}: ${res.body.slice(0, 300)}`);
  return res.body ? JSON.parse(res.body) : null;
}

export async function send_message(
  params: { space: string; text: string },
  ctx: Ctx,
): Promise<{ name: string }> {
  // `space` is the resource id like "spaces/AAAAAAA". send_message POSTs
  // to /v1/{space}/messages.
  const space = params.space.startsWith("spaces/") ? params.space : `spaces/${params.space}`;
  const result = (await chatApi("POST", `/${space}/messages`, { text: params.text }, ctx)) as { name: string };
  return { name: result.name };
}

export async function list_spaces(
  params: { limit?: number } | undefined,
  ctx: Ctx,
): Promise<{ spaces: Array<{ name: string; displayName: string; spaceType: string }> }> {
  const limit = params?.limit ?? 50;
  const result = (await chatApi("GET", `/spaces?pageSize=${limit}`, undefined, ctx)) as {
    spaces?: Array<{ name: string; displayName?: string; spaceType?: string }>;
  };
  return {
    spaces: (result.spaces ?? []).map((s) => ({
      name: s.name,
      displayName: s.displayName ?? "(no name)",
      spaceType: s.spaceType ?? "",
    })),
  };
}

export async function list_messages(
  params: { space: string; limit?: number },
  ctx: Ctx,
): Promise<{ messages: Array<{ name: string; sender: string; text: string; createdAt: string }> }> {
  const limit = params.limit ?? 30;
  const space = params.space.startsWith("spaces/") ? params.space : `spaces/${params.space}`;
  const result = (await chatApi("GET", `/${space}/messages?pageSize=${limit}&orderBy=createTime%20desc`, undefined, ctx)) as {
    messages?: Array<{ name: string; sender?: { displayName?: string; name?: string }; text?: string; createTime?: string }>;
  };
  return {
    messages: (result.messages ?? []).map((m) => ({
      name: m.name,
      sender: m.sender?.displayName ?? m.sender?.name ?? "unknown",
      text: m.text ?? "",
      createdAt: m.createTime ?? "",
    })),
  };
}

export const tools = [
  { name: "send_message", description: "Send a message to a Google Chat space. `space` is either the full resource id (spaces/ABCDEF) or just the id.", parameters: { type: "object", properties: { space: { type: "string" }, text: { type: "string" } }, required: ["space", "text"] } },
  { name: "list_spaces", description: "List Google Chat spaces (DMs + rooms) the authenticated user is a member of.", parameters: { type: "object", properties: { limit: { type: "number" } }, required: [] } },
  { name: "list_messages", description: "Read recent messages from a space, newest first.", parameters: { type: "object", properties: { space: { type: "string" }, limit: { type: "number" } }, required: ["space"] } },
];
