package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/exomind-tmi/whatsapp-mcp/internal/qr"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// loginBackend is what the login pages need from the account manager.
type loginBackend interface {
	// ValidLogin checks a nonce and does nothing else.
	ValidLogin(nick, nonce string) bool
	// Login checks it, starts the pairing on the first call, and returns the
	// pairing's state.
	Login(ctx context.Context, nick, nonce string) (wa.LoginState, error)
}

// loginHandler serves the QR login page under /login/. It is
// outside the bearer check, as a browser opens it: the nonce in the link,
// which the add tool hands out, is the capability. So that it
// cannot be told from a wrong guess, every reason to refuse is the same 404.
type loginHandler struct {
	backend loginBackend
	log     *slog.Logger
}

func newLoginHandler(b loginBackend, log *slog.Logger) *loginHandler {
	return &loginHandler{backend: b, log: log}
}

// ServeHTTP handles /login/<nick>, the page, and /login/<nick>/state, the
// page's poll. It writes no access log: the URL holds the nonce. The Host of the
// request is the daemon's own: the mux checks it for every endpoint
// (onlyOwnHost).
//
// Only the poll starts the pairing, not the page: something that merely
// fetches the link, a chat's link preview or a browser's prefetch, runs no
// script, and must not start a pairing whose code then expires unseen.
func (h *loginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setLoginHeaders(w.Header())
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nick, state, ok := parseLoginPath(r.URL)
	if !ok {
		http.NotFound(w, r)
		return
	}
	nonce := r.URL.Query().Get("t")
	if !state {
		if !h.backend.ValidLogin(nick, nonce) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, loginPage)
		return
	}
	st, err := h.backend.Login(r.Context(), nick, nonce)
	switch {
	case errors.Is(err, wa.ErrNoLogin):
		http.NotFound(w, r)
	case err != nil:
		if r.Context().Err() == nil {
			h.log.Warn("login state", "account", nick, "err", err)
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		h.writeState(w, nick, st)
	}
}

// parseLoginPath splits /login/<nick> and /login/<nick>/state; ok is false
// for anything else, such as a nick that cannot exist. A path that needed
// escaping is refused as it is: a nick has no character that does.
func parseLoginPath(u *url.URL) (nick string, state, ok bool) {
	rest, found := strings.CutPrefix(u.Path, wa.LoginPath)
	if !found || u.RawPath != "" {
		return "", false, false
	}
	nick, tail, hasTail := strings.Cut(rest, "/")
	if !wa.ValidNick(nick) || (hasTail && tail != "state") {
		return "", false, false
	}
	return nick, hasTail, true
}

// stateJSON is what the page polls: the QR code as a base64 PNG, not as a
// URL, so that the page loads nothing from anywhere.
type stateJSON struct {
	State  string `json:"state"`
	QRPNG  string `json:"qr_png,omitempty"`
	Reason string `json:"reason,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (h *loginHandler) writeState(w http.ResponseWriter, nick string, st wa.LoginState) {
	out := stateJSON{State: st.State, Reason: st.Reason, Hint: st.Hint}
	if st.Code != "" {
		png, err := qr.PNG(st.Code)
		if err != nil {
			h.log.Warn("draw the QR code", "account", nick, "err", err) // not the code
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out.QRPNG = base64.StdEncoding.EncodeToString(png)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// setLoginHeaders is for every answer under /login/, errors included. No
// cookie and no CORS header is ever set: a page of another origin must not
// read an answer. The CSP lets the page run only its own two inline blocks,
// by hash, and load nothing but its data: image and its own poll.
func setLoginHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer") // the URL holds the nonce
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", loginCSP)
	h.Set("Cross-Origin-Resource-Policy", "same-origin") // no other site may embed an answer
}

var loginCSP = "default-src 'none'; img-src data:; connect-src 'self'; " +
	"style-src " + cspHash(loginStyle) + "; script-src " + cspHash(loginScript) + "; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// cspHash is the CSP source expression for an inline block with exactly this
// content.
func cspHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

var loginPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Link WhatsApp</title>
<style>` + loginStyle + `</style>
</head>
<body>
<main>
<h1 id="title">Connecting to WhatsApp…</h1>
<img id="qr" alt="QR code to scan in WhatsApp" hidden>
<p id="text"></p>
<p id="hint"></p>
</main>
<script>` + loginScript + `</script>
</body>
</html>
`

const loginStyle = `
:root { color-scheme: light dark; }
body { font: 16px/1.5 system-ui, sans-serif; margin: 0; padding: 2rem 1rem; }
main { max-width: 28rem; margin: 0 auto; text-align: center; }
h1 { font-size: 1.4rem; }
img { image-rendering: pixelated; max-width: 100%; height: auto; }
[hidden] { display: none; }
`

// The page needs only what the poll tells it: the QR code is redrawn when it
// changes, and the page stops polling once linking is over, for good or ill.
const loginScript = `
(function () {
  "use strict";
  var again = "Ask Claude to call manage-accounts add again for a new link.";
  var over = false, failures = 0;
  function el(id) { return document.getElementById(id); }
  function show(title, text, hint) {
    el("title").textContent = title;
    el("text").textContent = text || "";
    el("hint").textContent = hint || "";
  }
  function qr(png) {
    var img = el("qr");
    if (!png) { img.hidden = true; img.removeAttribute("src"); return; }
    var src = "data:image/png;base64," + png;
    if (img.getAttribute("src") !== src) { img.src = src; }
    img.hidden = false;
  }
  function finish(title, text, hint) { over = true; qr(""); show(title, text, hint); }
  function render(s) {
    switch (s.state) {
    case "code":
      qr(s.qr_png);
      show("Scan with WhatsApp", "On the phone open WhatsApp, Linked devices, Link a device, and scan this code.", s.hint);
      break;
    case "paired":
      qr("");
      show("Scanned", "Connecting the new device…", s.hint);
      break;
    case "done":
      finish("Linked", "The account is linked. You can close this tab.", s.hint);
      break;
    case "failed":
      finish("Linking failed", s.reason, s.hint ? s.hint + " " + again : again);
      break;
    default:
      qr("");
      show("Connecting to WhatsApp…", "", s.hint);
    }
  }
  function tick() {
    fetch(location.pathname + "/state" + location.search, { cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" })
      .then(function (r) {
        if (r.status === 404) {
          finish("This link no longer works", "It has expired, a newer link has replaced it, or whatsapp-mcp was restarted.", again);
          return null;
        }
        if (!r.ok) { throw new Error("status " + r.status); }
        return r.json();
      })
      .then(function (s) { if (s) { failures = 0; render(s); } })
      .catch(function () {
        if (++failures >= 3) { finish("Cannot reach whatsapp-mcp", "It may have been restarted.", again); }
      })
      .then(function () { if (!over) { setTimeout(tick, 2000); } });
  }
  tick();
})();
`
