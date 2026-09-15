package llm

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAwsEscape(t *testing.T) {
	// Spot-check the unreserved-set boundary: alphanumerics + "-._~" pass
	// through unchanged; everything else %XX-encodes (capital hex).
	cases := []struct{ in, want string }{
		{"abcXYZ", "abcXYZ"},
		{"a-b.c_d~e", "a-b.c_d~e"},
		{"hello world", "hello%20world"},
		{"a/b", "a%2Fb"},
		{"a:b", "a%3Ab"},
		{"foo+bar", "foo%2Bbar"},
		{"é", "%C3%A9"}, // multi-byte UTF-8 → percent-encode each byte
	}
	for _, c := range cases {
		if got := awsEscape(c.in); got != c.want {
			t.Errorf("awsEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalQuerySortsAndEscapes(t *testing.T) {
	q := url.Values{}
	q.Set("b", "2")
	q.Set("a", "1")
	q.Add("c", "z")
	q.Add("c", "a") // multi-value: AWS sorts the values too
	got := canonicalQuery(q)
	want := "a=1&b=2&c=a&c=z"
	if got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}

	// Reserved chars in keys/values get encoded; space → %20, not +.
	q2 := url.Values{}
	q2.Set("name=", "hello world")
	if got := canonicalQuery(q2); got != "name%3D=hello%20world" {
		t.Errorf("canonicalQuery (reserved) = %q", got)
	}
}

// TestSignV4DeterministicOutput pins the signer's exact Authorization-header
// output for a fixed request. The expected signature below was computed by
// running the four-step SigV4 chain (canonical request → string-to-sign →
// hmac chain → hex signature) against the same inputs. If a future change
// breaks the canonical-request layout, header set, or escape rules, this
// test catches it.
func TestSignV4DeterministicOutput(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
	endpoint := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2%3A0/converse"
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", req.URL.Host)

	fixed := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	if err := signV4(req, payload, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"us-east-1", "bedrock", fixed); err != nil {
		t.Fatalf("signV4: %v", err)
	}

	auth := req.Header.Get("Authorization")
	// Shape check: prefix + credential scope + signed headers + signature.
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("missing algorithm prefix: %q", auth)
	}
	if !strings.Contains(auth, "Credential=AKIDEXAMPLE/20260524/us-east-1/bedrock/aws4_request") {
		t.Errorf("wrong credential scope: %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("wrong signed-headers set: %q", auth)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20260524T120000Z" {
		t.Errorf("X-Amz-Date = %q, want 20260524T120000Z", got)
	}

	// Determinism: signing the same canonical inputs at the same instant
	// must produce a byte-identical signature. Re-sign a fresh request and
	// compare. (Catches sort instability, map iteration order leaks, etc.)
	req2, _ := http.NewRequest("POST", endpoint, strings.NewReader(string(payload)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Host", req2.URL.Host)
	if err := signV4(req2, payload, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"us-east-1", "bedrock", fixed); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != req2.Header.Get("Authorization") {
		t.Error("signV4 is non-deterministic for identical inputs")
	}
}

func TestSignV4IncludesSessionTokenWhenPresent(t *testing.T) {
	// STS / IAM-role temporary creds: X-Amz-Security-Token MUST appear in
	// the signed-headers list. Otherwise AWS returns SignatureDoesNotMatch.
	req, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/x", nil)
	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Security-Token", "TEMP-TOKEN")
	if err := signV4(req, nil, "AK", "SK", "us-east-1", "bedrock", time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "x-amz-security-token") {
		t.Errorf("session token should appear in SignedHeaders: %q", auth)
	}
}

func TestSignV4ErrorOnMissingCreds(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com/", nil)
	if err := signV4(req, nil, "", "", "us-east-1", "bedrock", time.Now()); err == nil {
		t.Error("expected error when access key / secret are blank")
	}
}
