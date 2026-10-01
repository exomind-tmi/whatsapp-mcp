package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

const manageAccountsDescription = "Manage linked WhatsApp accounts.\n" +
	"- `list` — accounts with status (`connected`, `reconnecting`, `needs_link`, `replaced`, `client_outdated`) and archive size.\n" +
	"- `add` — link a NEW account, or RE-LINK an existing one: call `add` with the SAME `account_id`. " +
	"Re-linking keeps the whole message archive. Use it when status is `needs_link` or `replaced`, or after the device was removed on the phone. " +
	"Without `phone` returns a local page with a QR code; with `phone` returns an 8-character pairing code to enter on the phone.\n" +
	"- `remove` — FORGET the account: unlinks the device AND PERMANENTLY DELETES this account's message archive from this computer. " +
	"Downloaded files are kept. To reconnect a broken account, do NOT use remove; use `add` with the same `account_id`."

var manageAccountsTool = &mcp.Tool{
	Name:        "manage-accounts",
	Description: manageAccountsDescription,
	InputSchema: schemaFor[manageIn](map[string][]any{"action": {"list", "add", "remove"}}),
}

type manageIn struct {
	Action    string `json:"action" jsonschema:"list, add or remove"`
	AccountID string `json:"account_id,omitempty" jsonschema:"account nickname: lowercase latin letters, digits, _ and -, up to 64; required for add and remove"`
	Phone     string `json:"phone,omitempty" jsonschema:"add only: phone number of the account in international format; returns a pairing code instead of a QR page"`
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
		t, err := d.WA.Link(ctx, in.AccountID, in.Phone)
		if err != nil {
			return nil, ManageOut{}, err
		}
		out := ManageOut{Status: string(wa.StatusLinking), LoginURL: t.LoginURL, PairCode: t.PairCode, ExpiresAt: isoTime(t.ExpiresAt)}
		if t.PairCode != "" {
			out.NextStep = "WhatsApp on the phone → Linked devices → Link with phone number, enter the code"
		} else {
			out.NextStep = "open the link and scan the QR code in WhatsApp → Linked devices"
		}
		return nil, out, nil
	case "remove":
		r, err := d.WA.Remove(ctx, in.AccountID)
		if err != nil {
			return nil, ManageOut{}, err
		}
		// Not a wa.Status: a removed account no longer exists, so no account
		// is ever in this state; it only reports what the call did.
		return nil, ManageOut{Status: "removed", Hint: r.Hint}, nil
	}
	return nil, ManageOut{}, fmt.Errorf("unknown action %q: use list, add or remove", in.Action)
}

func (d Deps) listAccounts(ctx context.Context) ManageOut {
	accs := d.WA.Accounts(ctx)
	out := ManageOut{Accounts: make([]AccountOut, 0, len(accs))}
	for _, a := range accs {
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
