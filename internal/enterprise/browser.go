package enterprise

import (
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/security"
	"net/http"
	"strings"
)

// finishBrowserLogin writes credentials only into a no-store, same-origin page,
// never a redirect URL. JSON encoding escapes HTML-significant characters.
func finishBrowserLogin(w http.ResponseWriter, r *http.Request, body map[string]interface{}) bool {
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	payload, _ := json.Marshal(body)
	nonce := security.GenerateID()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The generated body is fixed except JSON-escaped data. Inline script is
	// required to bridge the existing bearer-token UI without a URL fragment.
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="no-referrer"><title>Signed in to Fathom</title></head><body><p>Signed in. Opening Fathom…</p><script nonce="%s">
 const login=%s;
 localStorage.setItem("fantazm_device_token",login.token);
 localStorage.setItem("fantazm_device_meta",JSON.stringify({id:login.user.id,name:login.user.name||login.user.email||"SSO account"}));
 if(login.stepUpToken)sessionStorage.setItem("fathom_step_up",JSON.stringify({token:login.stepUpToken,expires:Date.now()+300000}));
 location.replace(login.stepUpToken?"/admin":"/");
 </script></body></html>`, nonce, payload)
	return true
}
