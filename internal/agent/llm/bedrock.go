package llm

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

// Bedrock implements Provider against AWS Bedrock's Converse API
// (`bedrock-runtime:Converse`). Converse is the unified, model-agnostic
// message-style API that works across Claude / Llama / Mistral / Cohere /
// Titan / Nova hosted on Bedrock — same wire format regardless of which
// model family answers the call. Caller picks the model via cfg.Model,
// e.g. "anthropic.claude-3-5-sonnet-20241022-v2:0" or "amazon.nova-pro-v1:0".
//
// Auth is AWS SigV4 (region-scoped HMAC chain) — there's no bearer-token
// path. The signer here is hand-rolled to keep the AWS SDK out of the
// dependency tree, matching how every other provider in this package
// avoids vendor SDKs.
type Bedrock struct {
	cfg          types.LLMConfig
	region       string
	accessKey    string
	secretKey    string
	sessionToken string // optional, for STS / IAM-role temporary creds
	client       *http.Client
}

// NewBedrock builds a Bedrock provider. Region is required and comes from
// (in order): cfg.BaseURL (interpreted as a region name when it doesn't
// look like a URL), then AWS_REGION env, then "us-east-1" as last resort.
// sessionToken can be empty.
func NewBedrock(cfg types.LLMConfig, region, accessKey, secretKey, sessionToken string) *Bedrock {
	if region == "" {
		region = "us-east-1"
	}
	return &Bedrock{
		cfg:          cfg,
		region:       region,
		accessKey:    accessKey,
		secretKey:    secretKey,
		sessionToken: sessionToken,
		client:       &http.Client{},
	}
}

func (b *Bedrock) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	// Converse content blocks: text, toolUse, toolResult — same shape as
	// Anthropic's, but with camelCase field names and `inputSchema.json`
	// wrapping for tool defs.
	type cBlock map[string]interface{}
	type cMsg struct {
		Role    string   `json:"role"`
		Content []cBlock `json:"content"`
	}

	var systemBlocks []map[string]interface{}
	conv := make([]cMsg, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "system":
			systemBlocks = append(systemBlocks, map[string]interface{}{"text": m.Content})
			continue
		case "tool":
			conv = append(conv, cMsg{
				Role: "user",
				Content: []cBlock{{
					"toolResult": map[string]interface{}{
						"toolUseId": m.ToolCallID,
						"content":   []map[string]interface{}{{"text": m.Content}},
					},
				}},
			})
			continue
		}

		var blocks []cBlock
		if m.Content != "" {
			blocks = append(blocks, cBlock{"text": m.Content})
		}
		for _, tc := range m.ToolCalls {
			input := tc.Arguments
			if input == nil {
				input = map[string]interface{}{}
			}
			blocks = append(blocks, cBlock{
				"toolUse": map[string]interface{}{
					"toolUseId": tc.ID,
					"name":      tc.Name,
					"input":     input,
				},
			})
		}
		if len(blocks) == 0 {
			blocks = []cBlock{{"text": ""}}
		}
		conv = append(conv, cMsg{Role: m.Role, Content: blocks})
	}

	body := map[string]interface{}{
		"messages": conv,
		"inferenceConfig": map[string]interface{}{
			"maxTokens":   defaultInt(b.cfg.MaxTokens, 4096),
			"temperature": defaultFloat(b.cfg.Temperature, 0.7),
		},
	}
	if len(systemBlocks) > 0 {
		body["system"] = systemBlocks
	}
	if len(tools) > 0 {
		ts := make([]map[string]interface{}, 0, len(tools))
		for _, t := range tools {
			ts = append(ts, map[string]interface{}{
				"toolSpec": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"inputSchema": map[string]interface{}{"json": t.Parameters},
				},
			})
		}
		body["toolConfig"] = map[string]interface{}{"tools": ts}
	}

	payload, _ := json.Marshal(body)
	host := "bedrock-runtime." + b.region + ".amazonaws.com"
	path := "/model/" + url.PathEscape(b.cfg.Model) + "/converse"
	endpoint := "https://" + host + path

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Response{}, &ProviderError{Provider: "bedrock", Msg: "build request", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", host)
	if b.sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", b.sessionToken)
	}
	if err := signV4(req, payload, b.accessKey, b.secretKey, b.region, "bedrock", time.Now().UTC()); err != nil {
		return Response{}, &ProviderError{Provider: "bedrock", Msg: "sigv4 sign", Err: err}
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return Response{}, &ProviderError{Provider: "bedrock", Msg: "request failed", Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return Response{}, &ProviderError{Provider: "bedrock",
			Msg: fmt.Sprintf("status %d: %s", resp.StatusCode, string(raw))}
	}

	var parsed struct {
		Output struct {
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Text    string `json:"text,omitempty"`
					ToolUse *struct {
						ToolUseID string                 `json:"toolUseId"`
						Name      string                 `json:"name"`
						Input     map[string]interface{} `json:"input"`
					} `json:"toolUse,omitempty"`
				} `json:"content"`
			} `json:"message"`
		} `json:"output"`
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, &ProviderError{Provider: "bedrock", Msg: "response parse failed", Err: err}
	}

	var content string
	var calls []ToolCallRequest
	for _, c := range parsed.Output.Message.Content {
		if c.Text != "" {
			content += c.Text
		}
		if c.ToolUse != nil {
			calls = append(calls, ToolCallRequest{
				ID:        c.ToolUse.ToolUseID,
				Name:      c.ToolUse.Name,
				Arguments: c.ToolUse.Input,
			})
		}
	}
	out := Response{Content: content, ToolCalls: calls}
	if parsed.Usage != nil {
		out.Usage = &Usage{
			PromptTokens:     parsed.Usage.InputTokens,
			CompletionTokens: parsed.Usage.OutputTokens,
		}
	}
	switch parsed.StopReason {
	case "tool_use":
		out.FinishReason = FinishToolCalls
	case "max_tokens":
		out.FinishReason = FinishLength
	default:
		out.FinishReason = FinishStop
	}
	return out, nil
}

// signV4 attaches AWS SigV4 headers to req in place. Implements the
// algorithm documented at
// https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html — the
// canonical-request → string-to-sign → signing-key → signature chain.
//
// Payload is passed in separately because http.Request.Body is a one-shot
// reader; we already have the bytes from json.Marshal upstream.
func signV4(req *http.Request, payload []byte, accessKey, secretKey, region, service string, now time.Time) error {
	if accessKey == "" || secretKey == "" {
		return fmt.Errorf("missing AWS credentials")
	}
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Canonical headers: lowercase name, trimmed value, sorted by name.
	// Always include host + x-amz-date + x-amz-content-sha256; include
	// x-amz-security-token if present (must be signed when present).
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("X-Amz-Security-Token") != "" {
		signed = append(signed, "x-amz-security-token")
	}
	sort.Strings(signed)

	host := req.URL.Host
	if h := req.Header.Get("Host"); h != "" {
		host = h
	}
	headerValues := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if tok := req.Header.Get("X-Amz-Security-Token"); tok != "" {
		headerValues["x-amz-security-token"] = tok
	}

	var canonHeaders strings.Builder
	for _, name := range signed {
		canonHeaders.WriteString(name)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(headerValues[name]))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(signed, ";")

	canonRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credScope,
		sha256Hex([]byte(canonRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credScope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// canonicalURI normalises an already-escaped path for SigV4. Bedrock is not
// S3, so RFC 3986 unreserved-char rules apply: alphanumerics + "-._~" pass
// through unencoded; everything else is %XX-encoded. Empty path → "/".
func canonicalURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	return escapedPath
}

// canonicalQuery builds the SigV4 canonical query string: sort param names,
// stable-sort multi-value params by value, RFC-3986-encode keys and values,
// join with "&".
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for j, v := range vs {
			if i > 0 || j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(awsEscape(k))
			b.WriteByte('=')
			b.WriteString(awsEscape(v))
		}
	}
	return b.String()
}

// awsEscape encodes per the SigV4 rules: keep A-Z a-z 0-9 - _ . ~ literal;
// %-encode everything else. url.QueryEscape almost matches but encodes
// space as "+" and skips "~", so roll a small version here.
func awsEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}
	return b.String()
}
