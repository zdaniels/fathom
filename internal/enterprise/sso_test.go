package enterprise

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOIDCLoginAndStepUp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer, nonce, challenge, scenario string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]interface{}{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]interface{}{"keys": []interface{}{map[string]string{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		case "/token":
			r.ParseForm()
			hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
				t.Error("PKCE verifier mismatch")
				http.Error(w, "bad verifier", 400)
				return
			}
			claims := map[string]interface{}{"iss": issuer, "sub": "subject-123", "aud": "fathom", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce, "auth_time": time.Now().Unix()}
			switch scenario {
			case "wrong-audience":
				claims["aud"] = "other"
			case "wrong-issuer":
				claims["iss"] = "https://other.example"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			case "wrong-nonce":
				claims["nonce"] = "wrong"
			case "stale-auth":
				claims["auth_time"] = time.Now().Add(-time.Hour).Unix()
			case "missing-auth":
				delete(claims, "auth_time")
			}
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
			body, _ := json.Marshal(claims)
			payload := header + "." + base64.RawURLEncoding.EncodeToString(body)
			digest := sha256.Sum256([]byte(payload))
			sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if scenario == "bad-signature" {
				sig[0] ^= 1
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "access", "token_type": "Bearer", "id_token": payload + "." + base64.RawURLEncoding.EncodeToString(sig)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	s := NewSSOManager()
	if err := s.Configure(OIDCConfig{Issuer: issuer, ClientID: "fathom", RedirectURI: "http://127.0.0.1:8790/api/v1/sso/callback"}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		status int
		step   bool
	}{{"ok", 200, true}, {"wrong-audience", 401, false}, {"wrong-issuer", 401, false}, {"expired", 401, false}, {"wrong-nonce", 401, false}, {"bad-signature", 401, false}, {"stale-auth", 401, false}, {"missing-auth", 401, false}} {
		t.Run(test.name, func(t *testing.T) {
			scenario = test.name
			issued := 0
			issue := func(user string) (string, error) {
				issued++
				if !strings.HasPrefix(user, "oidc:") {
					t.Error("unscoped identity")
				}
				return "gateway-session", nil
			}
			admin := func(string) bool { return true }
			login := httptest.NewRecorder()
			s.Handle(login, httptest.NewRequest("GET", "/api/v1/sso/login", nil), issue, admin)
			if login.Code != 302 {
				t.Fatal(login.Body.String())
			}
			dest, _ := url.Parse(login.Header().Get("Location"))
			nonce = dest.Query().Get("nonce")
			challenge = dest.Query().Get("code_challenge")
			state := dest.Query().Get("state")
			if nonce == "" || challenge == "" || dest.Query().Get("code_challenge_method") != "S256" {
				t.Fatal("missing nonce/PKCE")
			}
			callback := func(cookie bool) *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", "/api/v1/sso/callback?code=code&state="+state, nil)
				if cookie {
					r.AddCookie(login.Result().Cookies()[0])
				}
				w := httptest.NewRecorder()
				s.Handle(w, r, issue, admin)
				return w
			}
			if w := callback(false); w.Code != 400 {
				t.Fatal("accepted callback without browser cookie")
			}
			w := callback(true)
			if w.Code != test.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if test.status == 200 {
				if issued != 1 {
					t.Fatal("session not issued")
				}
				var out map[string]interface{}
				json.Unmarshal(w.Body.Bytes(), &out)
				grant, _ := out["stepUpToken"].(string)
				if (grant != "") != test.step {
					t.Fatal("incorrect step-up eligibility")
				}
				if grant != "" {
					if _, ok := s.ConsumeStepUp(grant); !ok {
						t.Fatal("valid grant rejected")
					}
					if _, ok := s.ConsumeStepUp(grant); ok {
						t.Fatal("replayed grant accepted")
					}
				}
			} else if issued != 0 {
				t.Fatal("issued session for invalid ID token")
			}
			if w := callback(true); w.Code != 400 {
				t.Fatal("replayed login accepted")
			}
		})
	}
}
func TestSSORejectsInsecureConfiguration(t *testing.T) {
	s := NewSSOManager()
	if s.Configure(OIDCConfig{Issuer: "http://idp.example", ClientID: "x", RedirectURI: "http://host.example/api/v1/sso/callback"}) == nil {
		t.Fatal("insecure OIDC accepted")
	}
}
