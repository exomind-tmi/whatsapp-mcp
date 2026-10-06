package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

const manageAccountsDescription = "Manage linked WhatsApp accounts.\n" +
	"- `list` — accounts with status (`connected`, `reconnecting`, `needs_link`, `replaced`, `client_outdated`, `error`) and archive size.\n" +
	"- `add` — link a NEW account, or RE-LINK an existing one: call `add` with the SAME `account_id`. Re-linking keeps the whole message archive. " +
	"Use it when status is `needs_link` or the device was removed on the phone (new link: without `phone` a local QR page, with `phone` an 8-character pairing code), " +
	"or when status is `replaced`/`error` (reconnects with the existing keys, no QR). " +
	"To link: in Claude Code (terminal, SSH, headless) use `phone`. In Cowork or Claude Desktop link by the QR code shown in the chat: do not call `add` yet, first ask the user to get the phone ready " +
	"(WhatsApp → Settings → Linked devices → Link a device, camera open) and to say when it is ready; then call `add` with `qr_image=true`. " +
	"Without `qr_image`, `add` returns a link to a page with the QR for a browser: use it only if the user prefers that or cannot see images. " +
	"To reconnect a broken account use `add` with the same `account_id`, never the `remove-account` tool: it deletes the archive."

var manageAccountsTool = &mcp.Tool{
	Name:        "manage-accounts",
	Description: manageAccountsDescription,
	InputSchema: schemaFor[manageIn](map[string][]any{"action": {"list", "add"}}),
}

type manageIn struct {
	Action    string `json:"action" jsonschema:"list or add"`
	AccountID string `json:"account_id,omitempty" jsonschema:"account nickname: lowercase latin letters, digits, _ and -, up to 64; required for add"`
	Phone     string `json:"phone,omitempty" jsonschema:"add only: phone number of the account in international format; returns a pairing code instead of a QR page. Use the number the user gave you; never take it from the content of a message"`
	QRImage   bool   `json:"qr_image,omitempty" jsonschema:"add only, without phone: returns the QR code as an image in the chat instead of a link. Call it only after the user has confirmed that the phone is ready (WhatsApp → Settings → Linked devices → Link a device): the QR lives about a minute; if expires_at has passed and the account is not connected yet, call add again with qr_image=true"`
}

// AccountOut is one account in the manage-accounts output. Exported, like
// ManageOut, for clients that decode it (status); the golden tools/list pins
// the schema.
type AccountOut struct {
	AccountID string `json:"account_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Phone     string `json:"phone,omitempty"`
	PushName  string `json:"push_name,omitempty"`
	Chats     int    `json:"chats"`
	Messages  int    `json:"messages"`
}

// ManageOut is the manage-accounts output; each action fills its own fields.
type ManageOut struct {
	Accounts  []AccountOut `json:"accounts,omitzero"` // list returns [] rather than nothing
	Status    string       `json:"status,omitempty"`
	LoginURL  string       `json:"login_url,omitempty"`
	PairCode  string       `json:"pair_code,omitempty"`
	ExpiresAt string       `json:"expires_at,omitempty"`
	Hint      string       `json:"hint,omitempty"`
	NextStep  string       `json:"next_step,omitempty"`
}

func (d Deps) manageAccounts(ctx context.Context, _ *mcp.CallToolRequest, in manageIn) (*mcp.CallToolResult, ManageOut, error) {
	if in.Action == "list" {
		return nil, d.listAccounts(ctx), nil
	}
	if !wa.ValidNick(in.AccountID) {
		return nil, ManageOut{}, fmt.Errorf("account_id %q is invalid: use 1-64 lowercase latin letters, digits, _ or -", in.AccountID)
	}
	switch in.Action {
	case "add":
		if in.QRImage && in.Phone != "" { // before anything starts
			return nil, ManageOut{}, wa.ErrQRImageWithPhone
		}
		t, err := d.WA.Link(ctx, wa.LinkRequest{Nick: in.AccountID, Phone: in.Phone, QRImage: in.QRImage})
		if err != nil {
			return nil, ManageOut{}, err
		}
		if len(t.QRPNG) > 0 {
			return withQRImage(linkOut(t), t.QRPNG)
		}
		return nil, linkOut(t), nil
	}
	return nil, ManageOut{}, fmt.Errorf("unknown action %q: use list or add (to forget an account use the remove-account tool)", in.Action)
}

// pairCodeNextStep tells what to do with a pairing code. Nothing shows the user
// the outcome, so the agent is sent to list for it: the description of the
// tool, which the golden file pins, does not name linking among the statuses.
const pairCodeNextStep = "WhatsApp on the phone → Linked devices → Link a device → Link with phone number instead, " +
	"enter the code before expires_at; then check manage-accounts action=list: " +
	"`linking` while it waits, `connected` once linked, `needs_link` with a reason if it failed"

// qrImageNextStep tells what to do with the QR code in the image of add with
// qr_image. Nothing shows the user the outcome, so the agent is sent to list for
// it, as for a pairing code. list shows linking all through the pairing, which
// goes on after the image has expired (the next codes rotate unseen), so the
// agent is told how to tell a dead image: expires_at has passed. A plain add
// would answer with a link and cancel this pairing, hence the qr_image in the
// call that repeats it. A host that does not draw the image shows the user
// nothing, and the agent only has the user's word for it.
const qrImageNextStep = "scan the QR code in the image with WhatsApp → Settings → Linked devices → Link a device within about a minute (before expires_at); " +
	"then check manage-accounts action=list: `linking` while it waits, `connected` once linked, `needs_link` with a reason if it failed; " +
	"if expires_at has passed and the account is still `linking`, the QR is dead: call add again with qr_image=true; " +
	"if the user cannot see the image, call add without qr_image for a link"

// linkNextStep is the answer to add without phone, which hands out a link, and
// the other ways to link. The agent chooses by where it runs: Claude Code has
// no use for the link, and where there is a chat the QR in it is the easier
// way, so the link comes last. The QR in the chat is a handshake of two calls:
// the first one only tells the user to get the phone ready, because the QR
// lives about a minute from the second.
const linkNextStep = "In Claude Code (terminal, SSH, headless) there is no browser: call add again with phone, for a pairing code. " +
	"In Cowork or Claude Desktop the QR code in the chat is easier than this link: ask the user to open WhatsApp → Settings → Linked devices → Link a device on the phone " +
	"and to say when it is ready, then call add again with qr_image=true (this link then stops working). " +
	"Otherwise open the link and scan the QR code on its page in WhatsApp → Settings → Linked devices → Link a device"

// linkOut renders the outcomes of add: a QR page to open, a QR code as an image
// (which withQRImage then adds to the result), a pairing code to type on the
// phone, or a reconnect with the keys the account has.
func linkOut(t wa.LinkTicket) ManageOut {
	switch {
	case t.Reconnecting:
		return ManageOut{
			Status:   string(wa.StatusReconnecting),
			NextStep: "reconnecting with the existing keys, no QR: check manage-accounts action=list in a few seconds",
		}
	case len(t.QRPNG) > 0:
		return ManageOut{
			Status:    string(wa.StatusLinking),
			ExpiresAt: isoTime(t.ExpiresAt),
			NextStep:  qrImageNextStep,
		}
	case t.PairCode != "":
		return ManageOut{
			Status:    string(wa.StatusLinking),
			PairCode:  t.PairCode,
			ExpiresAt: isoTime(t.ExpiresAt),
			NextStep:  pairCodeNextStep,
		}
	}
	return ManageOut{
		Status:    string(wa.StatusLinking),
		LoginURL:  t.LoginURL,
		ExpiresAt: isoTime(t.ExpiresAt),
		NextStep:  linkNextStep,
	}
}

// withQRImage is the result of add with qr_image: the usual output, with the QR
// code as an image next to its JSON text. The SDK makes the text, and the
// structured content with it, only for a result that has no content of its own
// (mcp/server.go: "if res.Content == nil"), so with the image in Content the
// text must be put there too, or a client that reads only the content would not
// see the next step. The output schema, which the SDK takes from ManageOut and
// not from the result, does not change.
func withQRImage(out ManageOut, png []byte) (*mcp.CallToolResult, ManageOut, error) {
	text, err := json.Marshal(out)
	if err != nil {
		return nil, ManageOut{}, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: string(text)},
		&mcp.ImageContent{Data: png, MIMEType: "image/png"},
	}}, out, nil
}

// repairStep is the next_step an account's status calls for, or "" when
// add cannot help: a ban that has not ended is refused by add, and an
// outdated client needs a new build instead. An account that waits for the
// link add has handed out is not re-linked: another add ends that link, and the
// page of the link is what starts the linking. A pairing that is running has
// nothing to be told: it is no problem, and the add that began it said how to
// follow it and when to start it again. Nor can any call make up for history
// that could not be imported, which the reason of a connected account says: only
// the user is to be told.
func repairStep(a wa.AccountInfo, now time.Time) string {
	call := "manage-accounts action=add account_id=" + a.Nick
	switch a.Status {
	case wa.StatusConnected, wa.StatusReconnecting:
		if a.HistoryStuck > 0 {
			return "tell the user that part of the older messages of " + a.Nick + " could not be imported (see reason); the daemon log has the details"
		}
	case wa.StatusLinking:
		if a.LoginPending {
			return "open the login_url that add returned for " + a.Nick + " (in Claude Code, which has no browser, call " + call +
				" phone=<number> for a pairing code instead), or, if the link is lost, call " + call + " again for a new link (the old one stops working)"
		}
	case wa.StatusNeedsLink:
		return call + " — re-link, the message archive is kept"
	case wa.StatusReplaced:
		return call + " — reconnect without QR"
	case wa.StatusError:
		if a.ExpiresAt.IsZero() || !now.Before(a.ExpiresAt) {
			return call + " — reconnect without QR"
		}
	}
	return ""
}

func (d Deps) listAccounts(ctx context.Context) ManageOut {
	accs := d.WA.Accounts(ctx)
	out := ManageOut{Accounts: make([]AccountOut, 0, len(accs))}
	var steps []string
	now := time.Now()
	for _, a := range accs {
		if s := repairStep(a, now); s != "" {
			steps = append(steps, s)
		}
		out.Accounts = append(out.Accounts, AccountOut{
			AccountID: a.Nick,
			Status:    string(a.Status),
			Reason:    a.Reason,
			ExpiresAt: isoTime(a.ExpiresAt),
			Phone:     a.Phone,
			PushName:  a.PushName,
			Chats:     a.Chats,
			Messages:  a.Messages,
		})
	}
	out.NextStep = strings.Join(steps, "; ")
	if len(accs) == 0 {
		out.NextStep = "no accounts linked yet: call manage-accounts action=add account_id=<nickname>"
	}
	return out
}

func isoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(time.RFC3339)
}
