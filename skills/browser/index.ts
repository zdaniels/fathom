function extractTitle(html: string): string {
  const match = html.match(/<title[^>]*>([\s\S]*?)<\/title>/i);
  return match ? match[1].replace(/<[^>]+>/g, "").trim() : "";
}

function extractText(html: string): string {
  const text = html
    .replace(/<script[\s\S]*?<\/script>/gi, "")
    .replace(/<style[\s\S]*?<\/style>/gi, "")
    .replace(/<[^>]+>/g, " ")
    .replace(/\s+/g, " ")
    .trim();
  return text;
}

function extractLinks(html: string): string[] {
  const links: string[] = [];
  const regex = /<a[^>]+href\s*=\s*["']([^"']+)["'][^>]*>/gi;
  let m: RegExpExecArray | null;
  while ((m = regex.exec(html)) !== null) {
    links.push(m[1].trim());
  }
  return [...new Set(links)];
}

export async function navigate(params: {
  url: string;
}): Promise<{ title: string; url: string; textContent: string }> {
  const res = await fetch(params.url);
  if (!res.ok) throw new Error(`Fetch failed: ${res.status} ${res.statusText}`);
  const html = await res.text();
  return {
    title: extractTitle(html),
    url: params.url,
    textContent: extractText(html),
  };
}

export async function screenshot(params: {
  url: string;
}): Promise<{ message: string }> {
  return {
    message: `To take screenshots, install Playwright: npm install playwright. This skill is a scaffold; full screenshot support requires Playwright. Requested URL: ${params.url}`,
  };
}

export async function extractContent(params: {
  url: string;
  selector?: string;
}): Promise<{ textContent: string }> {
  const res = await fetch(params.url);
  if (!res.ok) throw new Error(`Fetch failed: ${res.status} ${res.statusText}`);
  let html = await res.text();
  if (params.selector) {
    const selectorRegex = new RegExp(
      `<[^>]+class\\s*=\\s*["'][^"']*${params.selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}[^"']*["'][^>]*>([\\s\\S]*?)<\\/\\w+>`,
      "i"
    );
    const m = html.match(selectorRegex);
    if (m) html = m[1];
  }
  return { textContent: extractText(html) };
}

export async function extractLinks(params: {
  url: string;
}): Promise<{ links: string[] }> {
  const res = await fetch(params.url);
  if (!res.ok) throw new Error(`Fetch failed: ${res.status} ${res.statusText}`);
  const html = await res.text();
  return { links: extractLinks(html) };
}

export const tools = [
  {
    name: "navigate",
    description: "Navigate to a URL and extract title and text content",
    parameters: {
      type: "object",
      properties: { url: { type: "string", description: "URL to navigate to" } },
      required: ["url"],
    },
  },
  {
    name: "screenshot",
    description: "Take a screenshot of a URL (requires Playwright install)",
    parameters: {
      type: "object",
      properties: { url: { type: "string", description: "URL to screenshot" } },
      required: ["url"],
    },
  },
  {
    name: "extract_content",
    description: "Extract text content from a URL, optionally by selector",
    parameters: {
      type: "object",
      properties: {
        url: { type: "string", description: "URL to extract from" },
        selector: { type: "string", description: "Optional CSS class selector" },
      },
      required: ["url"],
    },
  },
  {
    name: "extract_links",
    description: "Extract all href links from a URL",
    parameters: {
      type: "object",
      properties: { url: { type: "string", description: "URL to extract links from" } },
      required: ["url"],
    },
  },
];
