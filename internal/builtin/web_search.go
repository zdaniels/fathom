package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
)

// WebSearchTool hits DuckDuckGo's HTML lite endpoint — no API key required.
// We previously used the Instant Answer API which only returns definitional
// hits (great for "what is X", useless for news/general queries). Lite-HTML
// scraping returns real top results for any query.
func WebSearchTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "web_search",
		Description: "Search the web for information. Returns top results with title, URL, and snippet.",
		SkillName:   "web-search",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query":      map[string]interface{}{"type": "string", "description": "Search query"},
				"maxResults": map[string]interface{}{"type": "number", "description": "Max results (default 5)"},
			},
			"required": []string{"query"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			q, _ := p["query"].(string)
			max := 5
			if v, ok := p["maxResults"].(float64); ok && v > 0 {
				max = int(v)
			}
			if max > 20 {
				max = 20
			}
			if tctx.Policy != nil {
				if dec := tctx.Policy.CheckNetwork("duckduckgo.com"); dec.Decision == "deny" {
					return nil, fmt.Errorf("web_search blocked by policy: %s", dec.Reason)
				}
			}

			// First try Instant Answer — cheap, gives a clean abstract when
			// the query is definitional ("what is X").
			ia := iaLookup(ctx, q)

			// Then HTML lite for actual top results.
			results, err := liteSearch(ctx, q, max)
			if err != nil && len(ia) == 0 {
				return nil, fmt.Errorf("web_search: %w", err)
			}
			// IA hit first, then organic results, de-duped by URL.
			seen := make(map[string]bool)
			out := make([]map[string]string, 0, max+1)
			for _, r := range append(ia, results...) {
				if seen[r["url"]] {
					continue
				}
				seen[r["url"]] = true
				out = append(out, r)
				if len(out) >= max {
					break
				}
			}
			return map[string]interface{}{"results": out, "query": q}, nil
		},
	}
}

func iaLookup(ctx context.Context, q string) []map[string]string {
	endpoint := "https://api.duckduckgo.com/?q=" + url.QueryEscape(q) + "&format=json&no_html=1"
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, endpoint, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data struct {
		AbstractText string `json:"AbstractText"`
		AbstractURL  string `json:"AbstractURL"`
	}
	_ = json.Unmarshal(body, &data)
	if data.AbstractText != "" && data.AbstractURL != "" {
		return []map[string]string{{"title": q, "url": data.AbstractURL, "snippet": data.AbstractText}}
	}
	return nil
}

var liteResultRE = regexp.MustCompile(
	`(?s)<a rel="nofollow" href="([^"]+)"[^>]*>([^<]+)</a>.*?<td[^>]*class="result-snippet"[^>]*>([^<]*)</td>`,
)

func liteSearch(ctx context.Context, q string, max int) ([]map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	form := url.Values{"q": {q}, "kl": {"us-en"}}
	req, _ := http.NewRequestWithContext(cctx, http.MethodPost,
		"https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; FathomAgent/0.1)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	matches := liteResultRE.FindAllStringSubmatch(string(body), max)
	out := make([]map[string]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, map[string]string{
			"title":   strings.TrimSpace(html.UnescapeString(m[2])),
			"url":     unwrapDDG(m[1]),
			"snippet": strings.TrimSpace(html.UnescapeString(stripTags(m[3]))),
		})
	}
	return out, nil
}

// DDG sometimes wraps result links in /l/?uddg=<encoded-url>.
func unwrapDDG(u string) string {
	if i := strings.Index(u, "uddg="); i >= 0 {
		raw := u[i+5:]
		if amp := strings.Index(raw, "&"); amp >= 0 {
			raw = raw[:amp]
		}
		if dec, err := url.QueryUnescape(raw); err == nil {
			return dec
		}
	}
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

var tagRE = regexp.MustCompile(`<[^>]+>`)

func stripTags(s string) string { return tagRE.ReplaceAllString(s, "") }
