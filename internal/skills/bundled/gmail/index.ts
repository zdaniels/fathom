type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const BASE = "https://gmail.googleapis.com/gmail/v1";

const GMAIL_OAUTH_ATTACH = {
  type: "google-oauth" as const,
  refreshTokenSecret: "GMAIL_REFRESH_TOKEN",
  clientIdSecret: "GMAIL_CLIENT_ID",
  clientSecretSecret: "GMAIL_CLIENT_SECRET",
};

async function gmailApi(
  method: string,
  path: string,
  body: Record<string, unknown> | undefined,
  ctx: Ctx,
): Promise<unknown> {
  if (!ctx.fetch) throw new Error("gmail skill requires ctx.fetch.");
  const res = await ctx.fetch(`${BASE}${path}`, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
    attach: [GMAIL_OAUTH_ATTACH],
  });
  if (res.status >= 400) throw new Error(`Gmail API error (${res.status}): ${res.body.slice(0, 200)}`);
  return res.body ? (JSON.parse(res.body) as unknown) : undefined;
}

export async function listEmails(
  params: { query?: string; maxResults?: number },
  ctx: Ctx,
): Promise<{
  messages: Array<{ id: string; threadId: string; from: string; subject: string; date: string; snippet: string }>;
}> {
  const { query = "", maxResults = 10 } = params;
  const qs = new URLSearchParams({ maxResults: String(maxResults) });
  if (query) qs.set("q", query);
  const data = (await gmailApi("GET", `/users/me/messages?${qs}`, undefined, ctx)) as {
    messages?: Array<{ id: string; threadId: string }>;
  };
  const ids = data?.messages ?? [];
  // Enrich each message with From / Subject / Date headers + snippet via
  // format=metadata. One round-trip per message; cheaper than format=full
  // (no body payload). Run in parallel so a 10-message list stays fast.
  const enriched = await Promise.all(
    ids.map(async (m) => {
      try {
        const mp = new URLSearchParams();
        mp.set("format", "metadata");
        mp.append("metadataHeaders", "From");
        mp.append("metadataHeaders", "Subject");
        mp.append("metadataHeaders", "Date");
        const meta = (await gmailApi("GET", `/users/me/messages/${m.id}?${mp}`, undefined, ctx)) as {
          payload?: { headers?: Array<{ name: string; value: string }> };
          snippet?: string;
        };
        const headers = meta.payload?.headers ?? [];
        const getHeader = (name: string) =>
          headers.find((h) => h.name.toLowerCase() === name.toLowerCase())?.value ?? "";
        return {
          id: m.id,
          threadId: m.threadId,
          from: getHeader("From"),
          subject: getHeader("Subject"),
          date: getHeader("Date"),
          snippet: meta.snippet ?? "",
        };
      } catch {
        return { id: m.id, threadId: m.threadId, from: "", subject: "", date: "", snippet: "" };
      }
    }),
  );
  return { messages: enriched };
}

export async function readEmail(
  params: { messageId: string },
  ctx: Ctx,
): Promise<{ id: string; subject: string; from: string; body: string; snippet: string }> {
  const msg = (await gmailApi("GET", `/users/me/messages/${params.messageId}?format=full`, undefined, ctx)) as {
    id: string;
    payload: {
      headers: Array<{ name: string; value: string }>;
      body?: { data?: string };
      parts?: Array<{ mimeType: string; body?: { data?: string } }>;
    };
    snippet: string;
  };
  const getHeader = (name: string) =>
    msg.payload.headers.find((h) => h.name.toLowerCase() === name.toLowerCase())?.value ?? "";
  let body = "";
  if (msg.payload.body?.data) {
    body = Buffer.from(msg.payload.body.data, "base64").toString("utf-8");
  } else if (msg.payload.parts) {
    const textPart = msg.payload.parts.find((p) => p.mimeType === "text/plain" || p.mimeType === "text/html");
    if (textPart?.body?.data) {
      body = Buffer.from(textPart.body.data, "base64").toString("utf-8");
    }
  }
  return { id: msg.id, subject: getHeader("Subject"), from: getHeader("From"), body, snippet: msg.snippet };
}

export async function sendEmail(
  params: { to: string; subject: string; body: string },
  ctx: Ctx,
): Promise<{ id: string }> {
  const raw = Buffer.from(
    [
      `To: ${params.to}`,
      `Subject: ${params.subject}`,
      "Content-Type: text/plain; charset=utf-8",
      "",
      params.body,
    ].join("\r\n"),
  ).toString("base64url");
  const data = (await gmailApi("POST", "/users/me/messages/send", { raw }, ctx)) as { id: string };
  return data;
}

export async function searchEmails(
  params: { query: string },
  ctx: Ctx,
): Promise<{ results: Array<{ id: string; threadId: string }> }> {
  const list = await listEmails({ query: params.query, maxResults: 50 }, ctx);
  return { results: list.messages };
}

export const tools = [
  { name: "list_emails", description: "List Gmail messages", parameters: { type: "object", properties: { query: { type: "string", description: "Gmail search query" }, maxResults: { type: "number", description: "Default 10" } }, required: [] } },
  { name: "read_email", description: "Read a single email by ID", parameters: { type: "object", properties: { messageId: { type: "string" } }, required: ["messageId"] } },
  { name: "send_email", description: "Send an email", parameters: { type: "object", properties: { to: { type: "string" }, subject: { type: "string" }, body: { type: "string" } }, required: ["to", "subject", "body"] } },
  { name: "search_emails", description: "Search Gmail messages", parameters: { type: "object", properties: { query: { type: "string" } }, required: ["query"] } },
];
