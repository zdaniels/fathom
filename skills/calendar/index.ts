type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const BASE = "https://www.googleapis.com/calendar/v3";

const GOOGLE_OAUTH_ATTACH = {
  type: "google-oauth" as const,
  refreshTokenSecret: "GOOGLE_REFRESH_TOKEN",
  clientIdSecret: "GOOGLE_CLIENT_ID",
  clientSecretSecret: "GOOGLE_CLIENT_SECRET",
};

async function calApi(
  method: string,
  path: string,
  body: Record<string, unknown> | undefined,
  ctx: Ctx,
): Promise<unknown> {
  if (!ctx.fetch) throw new Error("calendar skill requires ctx.fetch.");
  const res = await ctx.fetch(`${BASE}${path}`, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
    attach: [GOOGLE_OAUTH_ATTACH],
  });
  if (res.status === 204) return undefined;
  if (res.status >= 400) throw new Error(`Calendar API error (${res.status}): ${res.body.slice(0, 200)}`);
  return res.body ? (JSON.parse(res.body) as unknown) : undefined;
}

export async function listEvents(
  params: { calendarId?: string; timeMin?: string; timeMax?: string; maxResults?: number },
  ctx: Ctx,
): Promise<{ items: Array<Record<string, unknown>> }> {
  const { calendarId = "primary", timeMin, timeMax, maxResults = 10 } = params;
  const qs = new URLSearchParams();
  if (timeMin) qs.set("timeMin", timeMin);
  if (timeMax) qs.set("timeMax", timeMax);
  qs.set("maxResults", String(maxResults));
  qs.set("singleEvents", "true");
  qs.set("orderBy", "startTime");
  const data = (await calApi("GET", `/calendars/${encodeURIComponent(calendarId)}/events?${qs}`, undefined, ctx)) as {
    items?: Array<Record<string, unknown>>;
  };
  return { items: data?.items ?? [] };
}

export async function createEvent(
  params: { summary: string; start: string; end: string; description?: string; location?: string },
  ctx: Ctx,
): Promise<{ id: string }> {
  const body: Record<string, unknown> = {
    summary: params.summary,
    start: { dateTime: params.start, timeZone: "UTC" },
    end: { dateTime: params.end, timeZone: "UTC" },
  };
  if (params.description) body.description = params.description;
  if (params.location) body.location = params.location;
  const data = (await calApi("POST", "/calendars/primary/events", body, ctx)) as { id: string };
  return data;
}

export async function updateEvent(
  params: { eventId: string; summary?: string; start?: string; end?: string },
  ctx: Ctx,
): Promise<{ id: string }> {
  const body: Record<string, unknown> = {};
  if (params.summary) body.summary = params.summary;
  if (params.start) body.start = { dateTime: params.start, timeZone: "UTC" };
  if (params.end) body.end = { dateTime: params.end, timeZone: "UTC" };
  const data = (await calApi(
    "PATCH",
    `/calendars/primary/events/${encodeURIComponent(params.eventId)}`,
    body,
    ctx,
  )) as { id: string };
  return data;
}

export async function deleteEvent(
  params: { eventId: string },
  ctx: Ctx,
): Promise<{ ok: true }> {
  await calApi(
    "DELETE",
    `/calendars/primary/events/${encodeURIComponent(params.eventId)}`,
    undefined,
    ctx,
  );
  return { ok: true };
}

export const tools = [
  { name: "list_events", description: "List Google Calendar events", parameters: { type: "object", properties: { calendarId: { type: "string" }, timeMin: { type: "string", description: "RFC3339 lower bound" }, timeMax: { type: "string", description: "RFC3339 upper bound" }, maxResults: { type: "number" } }, required: [] } },
  { name: "create_event", description: "Create a calendar event", parameters: { type: "object", properties: { summary: { type: "string" }, start: { type: "string" }, end: { type: "string" }, description: { type: "string" }, location: { type: "string" } }, required: ["summary", "start", "end"] } },
  { name: "update_event", description: "Update a calendar event", parameters: { type: "object", properties: { eventId: { type: "string" }, summary: { type: "string" }, start: { type: "string" }, end: { type: "string" } }, required: ["eventId"] } },
  { name: "delete_event", description: "Delete a calendar event", parameters: { type: "object", properties: { eventId: { type: "string" } }, required: ["eventId"] } },
];
