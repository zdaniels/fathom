package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
)

// ImageGenerateTool wraps OpenAI's image-generation API. Returns a path
// to the saved PNG. OpenAI is the only provider with a stable image API
// behind a simple secret; Anthropic doesn't have one yet. Routes through
// the agent's secret resolver so the key comes from the vault, not env.
//
// Costs real money per call (~$0.04 per image at standard quality, $0.17
// for high). The tool description warns the model.
func ImageGenerateTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "image_generate",
		Description: `Generate an image from a text prompt and save it locally. Returns the file path.
Backed by OpenAI's image API — requires OPENAI_API_KEY in the vault. Costs ~$0.04 per standard image, ~$0.17 for high quality. Use sparingly; not free.
Supported sizes: 1024x1024 (square), 1024x1536 (portrait), 1536x1024 (landscape). Quality: low | medium | high (default medium).`,
		SkillName: "image-generation",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt":  map[string]interface{}{"type": "string", "description": "Detailed description of the image to generate."},
				"size":    map[string]interface{}{"type": "string", "description": "1024x1024 | 1024x1536 | 1536x1024 (default 1024x1024)"},
				"quality": map[string]interface{}{"type": "string", "description": "low | medium | high (default medium)"},
				"path":    map[string]interface{}{"type": "string", "description": "Output PNG path (default: tempfile)."},
			},
			"required": []string{"prompt"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			prompt, _ := p["prompt"].(string)
			if prompt == "" {
				return nil, fmt.Errorf("image_generate: prompt is required")
			}
			size := stringOr(p, "size", "1024x1024")
			quality := stringOr(p, "quality", "medium")
			out := stringOr(p, "path", "")
			if out == "" {
				dir := filepath.Join(os.TempDir(), "fathom-images")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
				out = filepath.Join(dir, fmt.Sprintf("image_%d.png", time.Now().UnixNano()))
			}

			if tctx.GetSecret == nil {
				return nil, fmt.Errorf("image_generate: no secret resolver (run inside the agent loop)")
			}
			apiKey, err := tctx.GetSecret("OPENAI_API_KEY")
			if err != nil {
				return nil, fmt.Errorf("image_generate: OPENAI_API_KEY missing — `fathom vault set OPENAI_API_KEY <key>`")
			}

			body, _ := json.Marshal(map[string]interface{}{
				"model":   "gpt-image-1",
				"prompt":  prompt,
				"size":    size,
				"quality": quality,
				"n":       1,
			})
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(cctx, http.MethodPost,
				"https://api.openai.com/v1/images/generations", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, fmt.Errorf("image_generate: %w", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("image_generate: status %d: %s", resp.StatusCode, string(raw))
			}
			var parsed struct {
				Data []struct {
					B64JSON string `json:"b64_json"`
					URL     string `json:"url"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return nil, fmt.Errorf("image_generate: parse: %w", err)
			}
			if len(parsed.Data) == 0 {
				return nil, fmt.Errorf("image_generate: provider returned no image data")
			}
			var imgBytes []byte
			if b := parsed.Data[0].B64JSON; b != "" {
				imgBytes, err = base64.StdEncoding.DecodeString(b)
				if err != nil {
					return nil, fmt.Errorf("image_generate: base64 decode: %w", err)
				}
			} else if u := parsed.Data[0].URL; u != "" {
				// Some providers return a URL instead. Fetch it.
				imgReq, _ := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
				imgResp, err := http.DefaultClient.Do(imgReq)
				if err != nil {
					return nil, err
				}
				defer imgResp.Body.Close()
				imgBytes, _ = io.ReadAll(io.LimitReader(imgResp.Body, 16*1024*1024))
			} else {
				return nil, fmt.Errorf("image_generate: provider returned neither b64_json nor url")
			}
			if err := os.WriteFile(out, imgBytes, 0o644); err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"path":  out,
				"bytes": len(imgBytes),
				"size":  size,
			}, nil
		},
	}
}

func stringOr(p map[string]interface{}, key, def string) string {
	if v, ok := p[key].(string); ok && v != "" {
		return v
	}
	return def
}
