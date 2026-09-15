// Fathom browser skill — real Playwright automation.
//
// Three things make this useful to an LLM agent rather than just a "send
// HTTP" tool:
//
//  1. Persistent context across tool calls within a session. The skill keeps
//     a single Chromium browser + page open between invocations so the agent
//     can do multi-step flows (login → navigate → fill form → submit) without
//     losing state.
//  2. Snapshot tool returns a structured representation of the page —
//     visible text + interactive elements with stable selectors — so the LLM
//     can reason about what to click rather than receiving raw HTML.
//  3. Reading-mode extract strips chrome/nav/ads and returns the article
//     body. Useful for "summarize this page" without burning tokens on
//     boilerplate.
//
// Browser binary is auto-installed via `npx playwright install chromium`
// in the package.json's fantazm:postinstall script when you run
// `fathom install browser`. No manual setup.

import { chromium, type Browser, type BrowserContext, type Page } from "playwright";
import { mkdir, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

type Ctx = {
  getSecret?: (name: string) => string;
};

let _browser: Browser | null = null;
let _context: BrowserContext | null = null;
let _page: Page | null = null;

// Idle timeout — after this many ms of no activity, the browser is closed.
const IDLE_MS = 5 * 60 * 1000;
let _idleTimer: NodeJS.Timeout | null = null;

async function ensurePage(): Promise<Page> {
  if (_page && !_page.isClosed()) {
    resetIdle();
    return _page;
  }
  if (!_browser) {
    // Attach mode: when FANTAZM_BROWSER_CDP_URL is set, connect to a
    // user-launched Chrome instead of spawning headless Chromium. The user
    // launches their real Chrome with --remote-debugging-port=9222 (or
    // similar), then exports the URL. This means tools operate on the
    // logged-in browser session — same trick OpenClaw uses for "attach
    // to your real Chrome", just via CDP rather than a custom extension.
    const cdpURL = process.env.FANTAZM_BROWSER_CDP_URL;
    if (cdpURL) {
      _browser = await chromium.connectOverCDP(cdpURL);
      // When attached, reuse the existing default context so cookies +
      // logged-in sessions are inherited. Don't create a new BrowserContext.
      const ctxs = _browser.contexts();
      if (ctxs.length > 0) {
        _context = ctxs[0];
        const pages = _context.pages();
        if (pages.length > 0) {
          _page = pages[0];
          resetIdle();
          return _page;
        }
      }
    } else {
      _browser = await chromium.launch({
        headless: true,
        args: ["--no-sandbox", "--disable-blink-features=AutomationControlled"],
      });
    }
  }
  if (!_context) {
    _context = await _browser.newContext({
      userAgent:
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
      viewport: { width: 1280, height: 800 },
    });
  }
  _page = await _context.newPage();
  resetIdle();
  return _page;
}

function resetIdle() {
  if (_idleTimer) clearTimeout(_idleTimer);
  _idleTimer = setTimeout(() => {
    void teardown().catch(() => undefined);
  }, IDLE_MS);
}

async function teardown() {
  // In attach mode we must NOT close the user's real Chrome — only our
  // tracking refs. Closing would kill their browser session.
  const isAttached = !!process.env.FANTAZM_BROWSER_CDP_URL;
  try {
    if (!isAttached && _page && !_page.isClosed()) await _page.close();
  } catch {
    /* ignore */
  }
  try {
    if (!isAttached && _context) await _context.close();
  } catch {
    /* ignore */
  }
  try {
    if (!isAttached && _browser) await _browser.close();
  } catch {
    /* ignore */
  }
  _page = null;
  _context = null;
  _browser = null;
}

// --- Exported tools ---

export async function navigate(
  params: { url: string; waitFor?: "load" | "domcontentloaded" | "networkidle" },
  _ctx: Ctx,
): Promise<{ url: string; title: string; status: number | null }> {
  const page = await ensurePage();
  const resp = await page.goto(params.url, {
    waitUntil: params.waitFor ?? "domcontentloaded",
    timeout: 30000,
  });
  return {
    url: page.url(),
    title: await page.title(),
    status: resp?.status() ?? null,
  };
}

export async function snapshot(
  _params: Record<string, never> | undefined,
  _ctx: Ctx,
): Promise<{ url: string; title: string; text: string; interactive: Array<{ ref: string; role: string; text: string }> }> {
  const page = await ensurePage();
  const snap = await page.evaluate(() => {
    const w = window as unknown as { __fathomRefs?: Record<string, string> };
    w.__fathomRefs = {};
    let id = 0;
    const interactive: Array<{ ref: string; role: string; text: string }> = [];
    const selectors = ["a", "button", "input", "textarea", "select", "[role='button']", "[role='link']"];
    for (const sel of selectors) {
      for (const el of Array.from(document.querySelectorAll(sel))) {
        const r = (el as HTMLElement).getBoundingClientRect();
        if (r.width === 0 || r.height === 0) continue;
        const ref = `ref_${id++}`;
        const e = el as HTMLElement;
        let selector: string;
        if (e.id) selector = `#${CSS.escape(e.id)}`;
        else {
          const path: string[] = [];
          let cur: Element | null = e;
          while (cur && cur !== document.body && path.length < 6) {
            const tag = cur.tagName.toLowerCase();
            const idx = Array.from(cur.parentElement?.children ?? []).indexOf(cur) + 1;
            path.unshift(`${tag}:nth-child(${idx})`);
            cur = cur.parentElement;
          }
          selector = path.join(" > ");
        }
        w.__fathomRefs[ref] = selector;
        interactive.push({
          ref,
          role: e.getAttribute("role") ?? e.tagName.toLowerCase(),
          text: (e.innerText || (e as HTMLInputElement).value || e.getAttribute("aria-label") || e.getAttribute("placeholder") || "").trim().slice(0, 120),
        });
      }
    }
    const body = document.body?.innerText ?? "";
    return { text: body.replace(/\s+/g, " ").trim().slice(0, 8000), interactive };
  });
  return {
    url: page.url(),
    title: await page.title(),
    text: snap.text,
    interactive: snap.interactive.slice(0, 100),
  };
}

export async function click(
  params: { ref: string },
  _ctx: Ctx,
): Promise<{ ok: true }> {
  const page = await ensurePage();
  const selector = await page.evaluate((ref) => {
    const w = window as unknown as { __fathomRefs?: Record<string, string> };
    return w.__fathomRefs?.[ref];
  }, params.ref);
  if (!selector) throw new Error(`unknown ref ${params.ref} — call snapshot first to refresh refs`);
  await page.click(selector, { timeout: 10000 });
  return { ok: true };
}

export async function type_text(
  params: { ref: string; text: string; submit?: boolean },
  _ctx: Ctx,
): Promise<{ ok: true }> {
  const page = await ensurePage();
  const selector = await page.evaluate((ref) => {
    const w = window as unknown as { __fathomRefs?: Record<string, string> };
    return w.__fathomRefs?.[ref];
  }, params.ref);
  if (!selector) throw new Error(`unknown ref ${params.ref} — call snapshot first to refresh refs`);
  await page.fill(selector, params.text, { timeout: 10000 });
  if (params.submit) {
    await page.press(selector, "Enter");
  }
  return { ok: true };
}

export async function read_page(
  _params: Record<string, never> | undefined,
  _ctx: Ctx,
): Promise<{ url: string; title: string; content: string }> {
  const page = await ensurePage();
  const result = await page.evaluate(() => {
    const main =
      document.querySelector("article") ||
      document.querySelector("main") ||
      document.querySelector("[role='main']") ||
      document.body;
    const clone = main.cloneNode(true) as HTMLElement;
    for (const sel of ["nav", "header", "footer", "aside", ".ads", "[class*='advertisement']", "[class*='cookie']", "[class*='sidebar']"]) {
      for (const el of Array.from(clone.querySelectorAll(sel))) el.remove();
    }
    return {
      title: document.title,
      content: clone.innerText.replace(/\n\s*\n/g, "\n\n").trim().slice(0, 20000),
    };
  });
  return { url: page.url(), title: result.title, content: result.content };
}

export async function screenshot(
  params: { fullPage?: boolean; path?: string },
  _ctx: Ctx,
): Promise<{ path: string; bytes: number }> {
  const page = await ensurePage();
  let target = params.path;
  if (!target) {
    const dir = join(tmpdir(), "fathom-screenshots");
    await mkdir(dir, { recursive: true });
    target = join(dir, `shot_${Date.now()}.png`);
  }
  const buf = await page.screenshot({ fullPage: params.fullPage ?? false });
  await writeFile(target, buf);
  return { path: target, bytes: buf.byteLength };
}

export async function scroll(
  params: { direction?: "up" | "down" | "top" | "bottom"; amount?: number },
  _ctx: Ctx,
): Promise<{ ok: true; scrollY: number }> {
  const page = await ensurePage();
  const dir = params.direction ?? "down";
  const amount = params.amount ?? 600;
  const y = await page.evaluate(
    (args) => {
      const d = args.dir;
      const a = args.amount;
      if (d === "top") window.scrollTo({ top: 0, behavior: "instant" });
      else if (d === "bottom") window.scrollTo({ top: document.body.scrollHeight, behavior: "instant" });
      else if (d === "up") window.scrollBy({ top: -a, behavior: "instant" });
      else window.scrollBy({ top: a, behavior: "instant" });
      return window.scrollY;
    },
    { dir, amount },
  );
  return { ok: true, scrollY: y };
}

export async function close(
  _params: Record<string, never> | undefined,
  _ctx: Ctx,
): Promise<{ ok: true }> {
  await teardown();
  return { ok: true };
}

export const tools = [
  { name: "navigate", description: "Open a URL in the persistent browser. Returns final URL, title, HTTP status. The browser stays open between tool calls — subsequent navigate / click / snapshot calls operate on the same page.", parameters: { type: "object", properties: { url: { type: "string" }, waitFor: { type: "string", description: "load | domcontentloaded | networkidle (default domcontentloaded)" } }, required: ["url"] } },
  { name: "snapshot", description: "Get a structured view of the current page: visible text + list of interactive elements (links, buttons, inputs) each with a stable ref string. Use the refs from this snapshot when calling click / type_text.", parameters: { type: "object", properties: {}, required: [] } },
  { name: "click", description: "Click an interactive element by ref (from snapshot). Use snapshot first to discover refs.", parameters: { type: "object", properties: { ref: { type: "string" } }, required: ["ref"] } },
  { name: "type_text", description: "Type text into an input/textarea element by ref. Set submit=true to press Enter after.", parameters: { type: "object", properties: { ref: { type: "string" }, text: { type: "string" }, submit: { type: "boolean" } }, required: ["ref", "text"] } },
  { name: "read_page", description: "Reading-mode extraction: returns the article/main content with nav/footer/ads stripped. Use for 'summarize this page' style requests instead of snapshot.", parameters: { type: "object", properties: {}, required: [] } },
  { name: "screenshot", description: "Save a screenshot of the current page to a temp file. Returns the path.", parameters: { type: "object", properties: { fullPage: { type: "boolean", description: "Capture the entire scrollable page, not just the viewport" }, path: { type: "string", description: "Optional output path — defaults to a tempfile" } }, required: [] } },
  { name: "scroll", description: "Scroll the page. direction: up | down | top | bottom (default down). amount: pixels for up/down (default 600).", parameters: { type: "object", properties: { direction: { type: "string" }, amount: { type: "number" } }, required: [] } },
  { name: "close", description: "Close the browser explicitly. Normally Fathom closes it on idle timeout (5 min) so calling this is optional.", parameters: { type: "object", properties: {}, required: [] } },
];
