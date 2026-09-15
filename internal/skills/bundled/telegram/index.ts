type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const BASE = "https://api.telegram.org";

/**
 * Telegram's Bot API embeds the token in the URL path, so unlike Bearer-token
 * APIs the skill must construct the URL itself (the egress proxy can only
 * inject HEADERS, not URL fragments). The skill still goes through ctx.fetch
 * so the proxy enforces the allow-list on api.telegram.org — but the bot
 * token IS visible inside the skill subprocess. This is the weaker security
 * model we accept for this provider.
 */
async function botApi(
  method: string,
  params: Record<string, string | number> | undefined,
  ctx: Ctx,
): Promise<unknown> {
  if (!ctx.fetch || !ctx.getSecret) throw new Error("telegram skill requires ctx.fetch + ctx.getSecret.");
  const token = ctx.getSecret("TELEGRAM_BOT_TOKEN");
  const res = await ctx.fetch(`${BASE}/bot${token}/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: params ? JSON.stringify(params) : "{}",
  });
  if (res.status >= 400) throw new Error(`Telegram API error (${res.status}): ${res.body.slice(0, 200)}`);
  const data = JSON.parse(res.body) as { ok: boolean; result?: unknown; description?: string };
  if (!data.ok) throw new Error(data.description ?? "Telegram API error");
  return data.result;
}

export async function sendMessage(
  params: { chatId: string; text: string; parseMode?: string },
  ctx: Ctx,
): Promise<{ messageId: number }> {
  const body: Record<string, string | number> = { chat_id: params.chatId, text: params.text };
  if (params.parseMode) body.parse_mode = params.parseMode;
  const result = (await botApi("sendMessage", body, ctx)) as { message_id: number };
  return { messageId: result.message_id };
}

export async function getUpdates(
  params: { offset?: number; limit?: number } | undefined,
  ctx: Ctx,
): Promise<{ updates: Array<Record<string, unknown>> }> {
  const body: Record<string, number> = {};
  if (params?.offset != null) body.offset = params.offset;
  if (params?.limit != null) body.limit = params.limit;
  const result = (await botApi("getUpdates", body, ctx)) as Array<Record<string, unknown>>;
  return { updates: result };
}

export async function getMe(
  _params: Record<string, never> | undefined,
  ctx: Ctx,
): Promise<{ id: number; username: string; firstName: string }> {
  const result = (await botApi("getMe", undefined, ctx)) as { id: number; username: string; first_name: string };
  return { id: result.id, username: result.username, firstName: result.first_name };
}

export async function sendPhoto(
  params: { chatId: string; photoUrl: string; caption?: string },
  ctx: Ctx,
): Promise<{ messageId: number }> {
  const body: Record<string, string | number> = { chat_id: params.chatId, photo: params.photoUrl };
  if (params.caption) body.caption = params.caption;
  const result = (await botApi("sendPhoto", body, ctx)) as { message_id: number };
  return { messageId: result.message_id };
}

export const tools = [
  { name: "send_message", description: "Send a message via Telegram", parameters: { type: "object", properties: { chatId: { type: "string" }, text: { type: "string" }, parseMode: { type: "string", description: "HTML or Markdown" } }, required: ["chatId", "text"] } },
  { name: "get_updates", description: "Get incoming updates", parameters: { type: "object", properties: { offset: { type: "number" }, limit: { type: "number" } }, required: [] } },
  { name: "get_me", description: "Get bot information", parameters: { type: "object", properties: {}, required: [] } },
  { name: "send_photo", description: "Send a photo via Telegram", parameters: { type: "object", properties: { chatId: { type: "string" }, photoUrl: { type: "string" }, caption: { type: "string" } }, required: ["chatId", "photoUrl"] } },
];
