type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const BASE = "https://discord.com/api/v10";

const DISCORD_BOT_ATTACH = {
  type: "header" as const,
  name: "Authorization",
  secret: "DISCORD_BOT_TOKEN",
  template: "Bot ${secret}",
};

async function discordApi(
  method: string,
  path: string,
  body: Record<string, unknown> | undefined,
  ctx: Ctx,
): Promise<unknown> {
  if (!ctx.fetch) throw new Error("discord skill requires ctx.fetch.");
  const res = await ctx.fetch(`${BASE}${path}`, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
    attach: [DISCORD_BOT_ATTACH],
  });
  if (res.status >= 400) throw new Error(`Discord API error (${res.status}): ${res.body.slice(0, 200)}`);
  return res.body ? (JSON.parse(res.body) as unknown) : undefined;
}

export async function sendMessage(
  params: { channelId: string; content: string },
  ctx: Ctx,
): Promise<{ id: string }> {
  const result = (await discordApi("POST", `/channels/${params.channelId}/messages`, { content: params.content }, ctx)) as { id: string };
  return { id: result.id };
}

export async function readMessages(
  params: { channelId: string; limit?: number },
  ctx: Ctx,
): Promise<{ messages: Array<Record<string, unknown>> }> {
  const limit = params.limit ?? 50;
  const result = (await discordApi("GET", `/channels/${params.channelId}/messages?limit=${limit}`, undefined, ctx)) as Array<Record<string, unknown>>;
  return { messages: result };
}

export async function listGuilds(
  _params: Record<string, never> | undefined,
  ctx: Ctx,
): Promise<{ guilds: Array<Record<string, unknown>> }> {
  const result = (await discordApi("GET", "/users/@me/guilds", undefined, ctx)) as Array<Record<string, unknown>>;
  return { guilds: result };
}

export async function listChannels(
  params: { guildId: string },
  ctx: Ctx,
): Promise<{ channels: Array<Record<string, unknown>> }> {
  const result = (await discordApi("GET", `/guilds/${params.guildId}/channels`, undefined, ctx)) as Array<Record<string, unknown>>;
  return { channels: result };
}

export const tools = [
  { name: "send_message", description: "Send a message to a Discord channel", parameters: { type: "object", properties: { channelId: { type: "string" }, content: { type: "string" } }, required: ["channelId", "content"] } },
  { name: "read_messages", description: "Read messages from a Discord channel", parameters: { type: "object", properties: { channelId: { type: "string" }, limit: { type: "number", description: "Default 50" } }, required: ["channelId"] } },
  { name: "list_guilds", description: "List Discord guilds (servers) the bot is in", parameters: { type: "object", properties: {}, required: [] } },
  { name: "list_channels", description: "List channels in a Discord guild", parameters: { type: "object", properties: { guildId: { type: "string" } }, required: ["guildId"] } },
];
