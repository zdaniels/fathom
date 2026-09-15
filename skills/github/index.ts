type Ctx = {
  getSecret?: (name: string) => string;
  fetch?: (url: string, opts?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    attach?: unknown[];
  }) => Promise<{ status: number; statusText: string; headers: Record<string, string>; body: string }>;
};

const BASE = "https://api.github.com";

async function githubApi(
  method: string,
  path: string,
  body: Record<string, unknown> | undefined,
  ctx: Ctx,
): Promise<unknown> {
  if (!ctx.fetch) throw new Error("github skill requires ctx.fetch.");
  const url = path.startsWith("http") ? path : `${BASE}${path}`;
  const res = await ctx.fetch(url, {
    method,
    headers: {
      Accept: "application/vnd.github+json",
      "X-GitHub-Api-Version": "2022-11-28",
      "Content-Type": "application/json",
    },
    body: body ? JSON.stringify(body) : undefined,
    attach: [{ type: "bearer", secret: "GITHUB_TOKEN" }],
  });
  if (res.status >= 400) throw new Error(`GitHub API error (${res.status}): ${res.body.slice(0, 200)}`);
  return res.body ? (JSON.parse(res.body) as unknown) : undefined;
}

export async function listRepos(
  params: { org?: string; user?: string },
  ctx: Ctx,
): Promise<{ repos: Array<Record<string, unknown>> }> {
  const path = params.org
    ? `/orgs/${params.org}/repos`
    : params.user
      ? `/users/${params.user}/repos`
      : "/user/repos";
  const result = (await githubApi("GET", path, undefined, ctx)) as Array<Record<string, unknown>>;
  return { repos: result };
}

export async function getIssues(
  params: { owner: string; repo: string; state?: string },
  ctx: Ctx,
): Promise<{ issues: Array<Record<string, unknown>> }> {
  const qs = params.state ? `?state=${params.state}` : "";
  const result = (await githubApi("GET", `/repos/${params.owner}/${params.repo}/issues${qs}`, undefined, ctx)) as Array<Record<string, unknown>>;
  return { issues: result };
}

export async function createIssue(
  params: { owner: string; repo: string; title: string; body?: string; labels?: string[] },
  ctx: Ctx,
): Promise<{ number: number; htmlUrl: string }> {
  const body: Record<string, unknown> = { title: params.title };
  if (params.body) body.body = params.body;
  if (params.labels?.length) body.labels = params.labels;
  const result = (await githubApi("POST", `/repos/${params.owner}/${params.repo}/issues`, body, ctx)) as { number: number; html_url: string };
  return { number: result.number, htmlUrl: result.html_url };
}

export async function getPullRequests(
  params: { owner: string; repo: string; state?: string },
  ctx: Ctx,
): Promise<{ pullRequests: Array<Record<string, unknown>> }> {
  const qs = params.state ? `?state=${params.state}` : "";
  const result = (await githubApi("GET", `/repos/${params.owner}/${params.repo}/pulls${qs}`, undefined, ctx)) as Array<Record<string, unknown>>;
  return { pullRequests: result };
}

export async function searchCode(
  params: { query: string },
  ctx: Ctx,
): Promise<{ items: Array<Record<string, unknown>> }> {
  const result = (await githubApi("GET", `/search/code?q=${encodeURIComponent(params.query)}`, undefined, ctx)) as { items: Array<Record<string, unknown>> };
  return { items: result.items ?? [] };
}

export async function getNotifications(
  params: { all?: boolean },
  ctx: Ctx,
): Promise<{ notifications: Array<Record<string, unknown>> }> {
  const qs = params.all ? "?all=true" : "";
  const result = (await githubApi("GET", `/notifications${qs}`, undefined, ctx)) as Array<Record<string, unknown>>;
  return { notifications: result };
}

export async function createRepo(
  params: { name: string; org?: string; private?: boolean; description?: string; autoInit?: boolean },
  ctx: Ctx,
): Promise<{ fullName: string; htmlUrl: string; defaultBranch: string }> {
  const path = params.org ? `/orgs/${params.org}/repos` : "/user/repos";
  const body: Record<string, unknown> = {
    name: params.name,
    // Default private — Fathom is privacy-first; caller must opt into public.
    private: params.private ?? true,
    // auto_init creates an initial commit + default branch so a subsequent
    // create_or_update_file has a branch to write against.
    auto_init: params.autoInit ?? true,
  };
  if (params.description) body.description = params.description;
  const result = (await githubApi("POST", path, body, ctx)) as {
    full_name: string;
    html_url: string;
    default_branch: string;
  };
  return { fullName: result.full_name, htmlUrl: result.html_url, defaultBranch: result.default_branch };
}

export async function createOrUpdateFile(
  params: { owner: string; repo: string; path: string; content: string; message: string; branch?: string },
  ctx: Ctx,
): Promise<{ commitUrl: string; contentUrl: string; updated: boolean }> {
  // The Contents API needs the existing file's sha to UPDATE; its absence
  // means CREATE. Look it up first; a 404 (githubApi throws) → create.
  let sha: string | undefined;
  const refQs = params.branch ? `?ref=${encodeURIComponent(params.branch)}` : "";
  try {
    const existing = (await githubApi(
      "GET",
      `/repos/${params.owner}/${params.repo}/contents/${params.path}${refQs}`,
      undefined,
      ctx,
    )) as { sha?: string };
    sha = existing?.sha;
  } catch {
    // not found — we'll create it
  }
  const body: Record<string, unknown> = {
    message: params.message,
    content: Buffer.from(params.content, "utf-8").toString("base64"),
  };
  if (params.branch) body.branch = params.branch;
  if (sha) body.sha = sha;
  const result = (await githubApi(
    "PUT",
    `/repos/${params.owner}/${params.repo}/contents/${params.path}`,
    body,
    ctx,
  )) as { commit: { html_url: string }; content: { html_url: string } };
  return { commitUrl: result.commit.html_url, contentUrl: result.content.html_url, updated: Boolean(sha) };
}

export const tools = [
  { name: "list_repos", description: "List GitHub repositories", parameters: { type: "object", properties: { org: { type: "string" }, user: { type: "string" } }, required: [] } },
  { name: "get_issues", description: "Get issues for a repository", parameters: { type: "object", properties: { owner: { type: "string" }, repo: { type: "string" }, state: { type: "string", description: "open|closed|all" } }, required: ["owner", "repo"] } },
  { name: "create_issue", description: "Create a new issue", parameters: { type: "object", properties: { owner: { type: "string" }, repo: { type: "string" }, title: { type: "string" }, body: { type: "string" }, labels: { type: "array", items: { type: "string" } } }, required: ["owner", "repo", "title"] } },
  { name: "get_pull_requests", description: "Get pull requests for a repository", parameters: { type: "object", properties: { owner: { type: "string" }, repo: { type: "string" }, state: { type: "string", description: "open|closed|all" } }, required: ["owner", "repo"] } },
  { name: "search_code", description: "Search code on GitHub", parameters: { type: "object", properties: { query: { type: "string" } }, required: ["query"] } },
  { name: "get_notifications", description: "Get GitHub notifications", parameters: { type: "object", properties: { all: { type: "boolean" } }, required: [] } },
  { name: "create_repo", description: "Create a new GitHub repository (private by default)", parameters: { type: "object", properties: { name: { type: "string" }, org: { type: "string", description: "Org to create under; omit for your personal account" }, private: { type: "boolean", description: "default true" }, description: { type: "string" }, autoInit: { type: "boolean", description: "create an initial commit + default branch; default true" } }, required: ["name"] } },
  { name: "create_or_update_file", description: "Create or update a file in a repo and commit it (write+push via the GitHub API, no local clone needed)", parameters: { type: "object", properties: { owner: { type: "string" }, repo: { type: "string" }, path: { type: "string", description: "file path in the repo, e.g. src/app.py" }, content: { type: "string", description: "full file contents (UTF-8 text)" }, message: { type: "string", description: "commit message" }, branch: { type: "string", description: "target branch; defaults to the repo default" } }, required: ["owner", "repo", "path", "content", "message"] } },
];
