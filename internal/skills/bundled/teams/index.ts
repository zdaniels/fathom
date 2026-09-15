// Fathom Teams skill — Microsoft Graph API.
//
// Auth: OAuth 2.0 with Microsoft Identity Platform. Like Google's flow we
// store CLIENT_ID + CLIENT_SECRET + a long-lived REFRESH_TOKEN in the
// vault; the egress proxy exchanges the refresh token for an access token
// per request and injects the Bearer header. The skill never holds either
// raw token.
//
// One Microsoft-ism: many Graph endpoints require a TENANT_ID in the auth
// URL (`/v2.0/{tenant}/token`). We accept either a specific tenant ID or
// "common" for personal accounts. Stored as TEAMS_TENANT_ID — defaults to
// "common" if absent.

const BASE = "https://graph.microsoft.com/v1.0";

type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const TEAMS_ATTACH = {
  type: "microsoft-oauth" as const,
  refreshTokenSecret: "TEAMS_REFRESH_TOKEN",
  clientIdSecret: "TEAMS_CLIENT_ID",
  clientSecretSecret: "TEAMS_CLIENT_SECRET",
  tenantSecret: "TEAMS_TENANT_ID",
};

async function graph(method: string, path: string, body: unknown | undefined, ctx: Ctx): Promise<unknown> {
  if (!ctx.fetch) throw new Error("teams skill requires ctx.fetch");
  const res = await ctx.fetch(`${BASE}${path}`, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
    attach: [TEAMS_ATTACH],
  });
  if (res.status >= 400) throw new Error(`Graph API ${res.status}: ${res.body.slice(0, 300)}`);
  if (!res.body) return null;
  return JSON.parse(res.body);
}

export async function send_chat_message(
  params: { chatId: string; text: string },
  ctx: Ctx,
): Promise<{ id: string }> {
  const result = (await graph("POST", `/chats/${params.chatId}/messages`, {
    body: { contentType: "text", content: params.text },
  }, ctx)) as { id: string };
  return { id: result.id };
}

export async function list_chats(
  params: { limit?: number } | undefined,
  ctx: Ctx,
): Promise<{ chats: Array<{ id: string; topic: string; chatType: string; lastUpdatedAt: string }> }> {
  const limit = params?.limit ?? 20;
  const result = (await graph("GET", `/me/chats?$top=${limit}&$orderby=lastUpdatedDateTime desc`, undefined, ctx)) as {
    value: Array<{ id: string; topic?: string; chatType: string; lastUpdatedDateTime: string }>;
  };
  return {
    chats: result.value.map((c) => ({
      id: c.id,
      topic: c.topic ?? "(no topic)",
      chatType: c.chatType,
      lastUpdatedAt: c.lastUpdatedDateTime,
    })),
  };
}

export async function list_recent_messages(
  params: { chatId: string; limit?: number },
  ctx: Ctx,
): Promise<{ messages: Array<{ id: string; from: string; text: string; createdAt: string }> }> {
  const limit = params.limit ?? 20;
  const result = (await graph("GET", `/chats/${params.chatId}/messages?$top=${limit}`, undefined, ctx)) as {
    value: Array<{ id: string; from?: { user?: { displayName?: string } }; body?: { content?: string }; createdDateTime: string }>;
  };
  return {
    messages: result.value.map((m) => ({
      id: m.id,
      from: m.from?.user?.displayName ?? "system",
      text: stripHtml(m.body?.content ?? ""),
      createdAt: m.createdDateTime,
    })),
  };
}

export async function send_channel_message(
  params: { teamId: string; channelId: string; text: string },
  ctx: Ctx,
): Promise<{ id: string }> {
  const result = (await graph("POST", `/teams/${params.teamId}/channels/${params.channelId}/messages`, {
    body: { contentType: "text", content: params.text },
  }, ctx)) as { id: string };
  return { id: result.id };
}

export async function list_joined_teams(
  _params: Record<string, never> | undefined,
  ctx: Ctx,
): Promise<{ teams: Array<{ id: string; displayName: string; description: string }> }> {
  const result = (await graph("GET", `/me/joinedTeams`, undefined, ctx)) as {
    value: Array<{ id: string; displayName: string; description?: string }>;
  };
  return {
    teams: result.value.map((t) => ({
      id: t.id,
      displayName: t.displayName,
      description: t.description ?? "",
    })),
  };
}

export async function list_channels(
  params: { teamId: string },
  ctx: Ctx,
): Promise<{ channels: Array<{ id: string; displayName: string; description: string }> }> {
  const result = (await graph("GET", `/teams/${params.teamId}/channels`, undefined, ctx)) as {
    value: Array<{ id: string; displayName: string; description?: string }>;
  };
  return {
    channels: result.value.map((c) => ({
      id: c.id,
      displayName: c.displayName,
      description: c.description ?? "",
    })),
  };
}

// Strip simple HTML tags from Graph message body content (often comes
// back as HTML even for plain text messages). Doesn't try to be smart —
// the LLM just needs readable text.
function stripHtml(s: string): string {
  return s.replace(/<[^>]+>/g, " ").replace(/&nbsp;/g, " ").replace(/\s+/g, " ").trim();
}

export const tools = [
  { name: "send_chat_message", description: "Send a message in a 1:1 or group Teams chat. Use list_chats to find the chatId.", parameters: { type: "object", properties: { chatId: { type: "string" }, text: { type: "string" } }, required: ["chatId", "text"] } },
  { name: "list_chats", description: "List recent Teams chats (1:1 and group), most-recent first.", parameters: { type: "object", properties: { limit: { type: "number" } }, required: [] } },
  { name: "list_recent_messages", description: "Read recent messages from a specific chat.", parameters: { type: "object", properties: { chatId: { type: "string" }, limit: { type: "number" } }, required: ["chatId"] } },
  { name: "send_channel_message", description: "Post in a team channel. Needs teamId + channelId (list_joined_teams + list_channels).", parameters: { type: "object", properties: { teamId: { type: "string" }, channelId: { type: "string" }, text: { type: "string" } }, required: ["teamId", "channelId", "text"] } },
  { name: "list_joined_teams", description: "List the teams the authenticated user is a member of.", parameters: { type: "object", properties: {}, required: [] } },
  { name: "list_channels", description: "List the channels in a team.", parameters: { type: "object", properties: { teamId: { type: "string" } }, required: ["teamId"] } },
];
