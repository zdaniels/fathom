type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const BASE = "https://slack.com/api";

async function slackApi(
  method: string,
  params: Record<string, string | number | boolean> | undefined,
  ctx: Ctx,
): Promise<Record<string, unknown>> {
  if (!ctx.fetch) throw new Error("slack skill requires ctx.fetch — must be invoked through the sandbox runner.");
  const res = await ctx.fetch(`${BASE}/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: params ? JSON.stringify(params) : "{}",
    attach: [{ type: "bearer", secret: "SLACK_BOT_TOKEN" }],
  });
  if (res.status >= 400) throw new Error(`Slack API error (${res.status}): ${res.body.slice(0, 200)}`);
  const data = JSON.parse(res.body) as { ok: boolean; error?: string; [key: string]: unknown };
  if (!data.ok) throw new Error(data.error ?? "Slack API error");
  return data;
}

export async function listChannels(
  params: { limit?: number } | undefined,
  ctx: Ctx,
): Promise<{ channels: Array<{ id: string; name: string; is_channel: boolean }> }> {
  const body: Record<string, number> = {};
  if (params?.limit != null) body.limit = params.limit;
  const data = await slackApi("conversations.list", body, ctx);
  return { channels: (data.channels as Array<{ id: string; name: string; is_channel: boolean }>) ?? [] };
}

export async function sendMessage(
  params: { channel: string; text: string; threadTs?: string },
  ctx: Ctx,
): Promise<{ ts: string }> {
  const body: Record<string, string> = { channel: params.channel, text: params.text };
  if (params.threadTs) body.thread_ts = params.threadTs;
  const data = await slackApi("chat.postMessage", body, ctx);
  return { ts: String(data.ts ?? "") };
}

export async function readMessages(
  params: { channel: string; limit?: number },
  ctx: Ctx,
): Promise<{ messages: Array<Record<string, unknown>> }> {
  const { channel, limit = 10 } = params;
  const data = await slackApi("conversations.history", { channel, limit }, ctx);
  return { messages: (data.messages as Array<Record<string, unknown>>) ?? [] };
}

export async function listUsers(
  params: { limit?: number } | undefined,
  ctx: Ctx,
): Promise<{ members: Array<{ id: string; name: string; real_name?: string }> }> {
  const body: Record<string, number> = {};
  if (params?.limit != null) body.limit = params.limit;
  const data = await slackApi("users.list", body, ctx);
  return { members: (data.members as Array<{ id: string; name: string; real_name?: string }>) ?? [] };
}

export const tools = [
  {
    name: "list_channels",
    description: "List Slack channels",
    parameters: {
      type: "object",
      properties: {
        limit: { type: "number", description: "Maximum channels to return" },
      },
      required: [],
    },
  },
  {
    name: "send_message",
    description: "Send a message to a Slack channel",
    parameters: {
      type: "object",
      properties: {
        channel: { type: "string", description: "Channel ID" },
        text: { type: "string", description: "Message text" },
        threadTs: { type: "string", description: "Thread timestamp for replies" },
      },
      required: ["channel", "text"],
    },
  },
  {
    name: "read_messages",
    description: "Read messages from a Slack channel",
    parameters: {
      type: "object",
      properties: {
        channel: { type: "string", description: "Channel ID" },
        limit: { type: "number", description: "Maximum messages (default 10)" },
      },
      required: ["channel"],
    },
  },
  {
    name: "list_users",
    description: "List Slack workspace users",
    parameters: {
      type: "object",
      properties: {
        limit: { type: "number", description: "Maximum users to return" },
      },
      required: [],
    },
  },
];
