package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/pkg/types"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Connection is configured by the instance owner, scoped to exactly one
// workspace. Credentials are resolved server-side and never returned by APIs.
type Connection = types.TaskConnection

type Connector struct {
	Config Connection
	Lookup func(string) (string, error)
	Client *http.Client
}

var issueKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func (c *Connector) request(ctx context.Context, method, path string, body any, out any) error {
	cfg := c.Config
	if cfg.Scope == "" {
		return errors.New("connector requires a team/project scope")
	}
	token, err := c.Lookup(cfg.TokenSecret)
	if err != nil || token == "" {
		return errors.New("connector credential is unavailable")
	}
	endpoint := "https://api.linear.app/graphql"
	if cfg.Provider == "jira" {
		u, err := url.Parse(cfg.Site)
		if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".atlassian.net") || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("Jira site must be https://your-site.atlassian.net")
		}
		endpoint = strings.TrimRight(cfg.Site, "/") + path
	} else if cfg.Provider != "linear" {
		return errors.New("unsupported task provider")
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cfg.Provider == "linear" {
		req.Header.Set("Authorization", token)
	} else {
		req.SetBasicAuth(cfg.Email, token)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("connector redirects are refused") }}
	}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("task provider request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("task provider returned HTTP %d", res.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	return nil
}
func (c *Connector) Import(ctx context.Context, id string) (Task, error) {
	var t Task
	if !issueKey.MatchString(id) {
		return t, errors.New("enter an issue key or UUID")
	}
	t.Source = c.Config.Provider
	if t.Source == "linear" {
		var res struct {
			Data struct {
				Issue *struct {
					ID, Identifier, Title, Description, URL string
					Team                                    struct{ ID string }
				}
			}
			Errors []json.RawMessage
		}
		err := c.request(ctx, "POST", "", map[string]any{"query": `query($id:String!){issue(id:$id){id identifier title description url team{id}}}`, "variables": map[string]string{"id": id}}, &res)
		if err != nil {
			return t, err
		}
		if len(res.Errors) > 0 || res.Data.Issue == nil {
			return t, errors.New("Linear issue was not available")
		}
		i := res.Data.Issue
		if i.Team.ID != c.Config.Scope {
			return t, errors.New("issue is outside this workspace's configured Linear team")
		}
		t.Title = i.Title
		t.Description = i.Description
		t.ExternalID = i.ID
		t.ExternalURL = i.URL
	} else {
		var res struct {
			ID, Key string
			Fields  struct {
				Summary     string
				Description json.RawMessage
				Project     struct{ Key string }
			}
		}
		if err := c.request(ctx, "GET", "/rest/api/3/issue/"+url.PathEscape(id)+"?fields=summary,description,project", nil, &res); err != nil {
			return t, err
		}
		if res.Fields.Project.Key != c.Config.Scope {
			return t, errors.New("issue is outside this workspace's configured Jira project")
		}
		t.Title = res.Fields.Summary
		t.Description = adfText(res.Fields.Description)
		t.ExternalID = res.Key
		t.ExternalURL = strings.TrimRight(c.Config.Site, "/") + "/browse/" + url.PathEscape(res.Key)
	}
	return t, nil
}

// Publish adds a reviewable handoff comment. It does not change the external
// workflow state or overwrite a task description edited in Linear/Jira.
func (c *Connector) Publish(ctx context.Context, t Task, text string) error {
	if _, err := c.Import(ctx, t.ExternalID); err != nil {
		return err
	} // recheck scope before writing
	if c.Config.Provider == "linear" {
		var res struct {
			Data   struct{ CommentCreate struct{ Success bool } }
			Errors []json.RawMessage
		}
		err := c.request(ctx, "POST", "", map[string]any{"query": `mutation($issue:String!,$body:String!){commentCreate(input:{issueId:$issue,body:$body}){success}}`, "variables": map[string]string{"issue": t.ExternalID, "body": text}}, &res)
		if err != nil {
			return err
		}
		if len(res.Errors) > 0 || !res.Data.CommentCreate.Success {
			return errors.New("Linear did not confirm the comment")
		}
		return nil
	}
	body := map[string]any{"body": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text}}}}}}
	return c.request(ctx, "POST", "/rest/api/3/issue/"+url.PathEscape(t.ExternalID)+"/comment", body, nil)
}
func adfText(raw json.RawMessage) string {
	var node any
	if json.Unmarshal(raw, &node) != nil {
		return ""
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			b.WriteString(x)
		case map[string]any:
			if text, ok := x["text"].(string); ok {
				b.WriteString(text)
			}
			if children, ok := x["content"].([]any); ok {
				for _, child := range children {
					walk(child)
				}
			}
			if x["type"] == "paragraph" || x["type"] == "heading" {
				b.WriteString("\n")
			}
		}
	}
	walk(node)
	return strings.TrimSpace(b.String())
}
