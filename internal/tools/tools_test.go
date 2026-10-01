package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

func connect(t *testing.T, w WA) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := NewServer("v0.0.1", Deps{WA: w}).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "manage-accounts", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res, ResultText(res)
}

func TestServerAdvertisesInstructionsAndPinnedProtocol(t *testing.T) {
	cs := connect(t, &toolstest.WA{})
	init := cs.InitializeResult()
	if init.Instructions != Instructions {
		t.Error("instructions not advertised")
	}
	if init.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol = %s, want %s", init.ProtocolVersion, ProtocolVersion)
	}
}

func TestManageAccountsSchema(t *testing.T) {
	cs := connect(t, &toolstest.WA{})
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "manage-accounts" {
		t.Fatalf("tools = %+v", list.Tools)
	}
	tool := list.Tools[0]
	if tool.Description != manageAccountsDescription {
		t.Error("the listed description differs from the one in the code")
	}
	// The scenarios plan 5.3 wants visible from the description alone.
	for _, want := range []string{
		"(`connected`, `reconnecting`, `needs_link`, `replaced`, `client_outdated`, `error`)",
		"call `add` with the SAME `account_id`. Re-linking keeps the whole message archive.",
		"when status is `needs_link` or the device was removed on the phone (new link: without `phone` a local QR page, with `phone` an 8-character pairing code)",
		"or when status is `replaced`/`error` (reconnects with the existing keys, no QR).",
		"FORGET the account: unlinks the device AND PERMANENTLY DELETES this account's message archive from this computer. Downloaded files are kept.",
		"To reconnect a broken account, do NOT use remove; use `add` with the same `account_id`.",
	} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description lacks %q (plan 5.3)", want)
		}
	}
	b, _ := json.Marshal(tool.InputSchema)
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	json.Unmarshal(b, &schema)
	if got := strings.Join(schema.Properties["action"].Enum, ","); got != "list,add,remove" {
		t.Errorf("action enum = %q", got)
	}
	if strings.Join(schema.Required, ",") != "action" {
		t.Errorf("required = %v", schema.Required)
	}
	if tool.OutputSchema == nil {
		t.Error("no output schema")
	}
}

var update = flag.Bool("update", false, "rewrite testdata/*.golden.json")

// TestToolsListGolden pins the exact schemas clients see: field names,
// omitempty, descriptions and jsonschema-go's layout are the contract.
func TestToolsListGolden(t *testing.T) {
	list, err := connect(t, &toolstest.WA{}).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(list.Tools, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "tools_list.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/tools -update)", err)
	}
	if !bytes.Equal(got, bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
		t.Errorf("tools/list differs from %s (if intended, rerun with -update):\n%s", golden, got)
	}
}

func TestEveryListedToolIsKnown(t *testing.T) {
	list, err := connect(t, &toolstest.WA{}).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != len(registry) {
		t.Errorf("tools/list has %d tools, registry %d", len(list.Tools), len(registry))
	}
	for _, tool := range list.Tools {
		if !Known(tool.Name) {
			t.Errorf("%s is served but not Known: the shim would not forward it", tool.Name)
		}
	}
}

func TestManageAccountsListEmpty(t *testing.T) {
	res, text := call(t, connect(t, &toolstest.WA{}), map[string]any{"action": "list"})
	if res.IsError {
		t.Fatalf("error: %s", text)
	}
	if !strings.Contains(text, `"accounts":[]`) || !strings.Contains(text, "action=add") {
		t.Fatalf("list = %s", text)
	}
}

// sameJSON compares JSON by value: the output contract is the fields and
// their values, not the key order the SDK happens to write.
func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(g, w)
}

// ExpiresAt is shown in local time, so the expectations format it the same way.
var expires = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func TestManageAccountsList(t *testing.T) {
	fake := &toolstest.WA{Accs: []wa.AccountInfo{
		{Nick: "personal", Status: wa.StatusConnected, Phone: "+70000000000", PushName: "Anton", Chats: 3, Messages: 42},
		{Nick: "work", Status: wa.StatusError, Reason: "temporarily banned", ExpiresAt: expires},
	}}
	_, text := call(t, connect(t, fake), map[string]any{"action": "list"})
	want := `{"accounts":[
		{"account_id":"personal","status":"connected","phone":"+70000000000","push_name":"Anton","chats":3,"messages":42},
		{"account_id":"work","status":"error","reason":"temporarily banned","expires_at":"` + expires.Local().Format(time.RFC3339) + `","chats":0,"messages":0}]}`
	if !sameJSON(t, text, want) {
		t.Fatalf("list = %s\nwant   %s", text, want)
	}
}

// TestManageAccountsListNextStep: a problem status carries the call that
// fixes it, with what that call does (plan 5.3); the others carry nothing.
func TestManageAccountsListNextStep(t *testing.T) {
	banned := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name string
		accs []wa.AccountInfo
		want string
	}{
		{"healthy", []wa.AccountInfo{{Nick: "a", Status: wa.StatusConnected}, {Nick: "b", Status: wa.StatusReconnecting}, {Nick: "c", Status: wa.StatusLinking}}, ""},
		{"needs_link", []wa.AccountInfo{{Nick: "a", Status: wa.StatusNeedsLink}},
			"manage-accounts action=add account_id=a — re-link, the message archive is kept"},
		{"replaced", []wa.AccountInfo{{Nick: "a", Status: wa.StatusReplaced}},
			"manage-accounts action=add account_id=a — reconnect without QR"},
		{"error", []wa.AccountInfo{{Nick: "a", Status: wa.StatusError, Reason: "connect failure"}},
			"manage-accounts action=add account_id=a — reconnect without QR"},
		{"ban over", []wa.AccountInfo{{Nick: "a", Status: wa.StatusError, ExpiresAt: time.Now().Add(-time.Minute)}},
			"manage-accounts action=add account_id=a — reconnect without QR"},
		{"banned", []wa.AccountInfo{{Nick: "a", Status: wa.StatusError, ExpiresAt: banned}}, ""},
		{"client_outdated", []wa.AccountInfo{{Nick: "a", Status: wa.StatusClientOutdated}}, ""},
		{"several", []wa.AccountInfo{
			{Nick: "a", Status: wa.StatusNeedsLink}, {Nick: "b", Status: wa.StatusConnected}, {Nick: "c", Status: wa.StatusReplaced}},
			"manage-accounts action=add account_id=a — re-link, the message archive is kept; manage-accounts action=add account_id=c — reconnect without QR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, text := call(t, connect(t, &toolstest.WA{Accs: tc.accs}), map[string]any{"action": "list"})
			var out ManageOut
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatal(err)
			}
			if out.NextStep != tc.want || len(out.Accounts) != len(tc.accs) {
				t.Errorf("next_step = %q, want %q (%d accounts)", out.NextStep, tc.want, len(out.Accounts))
			}
		})
	}
}

// TestManageAccountsAddRemoveOutput pins the JSON (the three outcomes of add among them) the client sees and what
// reaches WA; the golden file covers only the schemas.
func TestManageAccountsAddRemoveOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *toolstest.WA
		args map[string]any
		call toolstest.Call
		want string
	}{
		{"add qr", &toolstest.WA{Ticket: wa.LinkTicket{LoginURL: "http://127.0.0.1:1/login/x?t=n", ExpiresAt: expires}},
			map[string]any{"action": "add", "account_id": "personal"},
			toolstest.Call{Method: "Link", Nick: "personal"},
			`{"login_url":"http://127.0.0.1:1/login/x?t=n","expires_at":"` + expires.Local().Format(time.RFC3339) +
				`","next_step":"open the link and scan the QR code in WhatsApp → Linked devices","status":"linking"}`},
		{"add reconnect", &toolstest.WA{Ticket: wa.LinkTicket{Reconnecting: true}},
			map[string]any{"action": "add", "account_id": "personal"},
			toolstest.Call{Method: "Link", Nick: "personal"},
			`{"next_step":"reconnecting with the existing keys, no QR: check manage-accounts action=list in a few seconds","status":"reconnecting"}`},
		{"add code", &toolstest.WA{Ticket: wa.LinkTicket{PairCode: "ABCD-EFGH", ExpiresAt: expires}},
			map[string]any{"action": "add", "account_id": "personal", "phone": "+70000000000"},
			toolstest.Call{Method: "Link", Nick: "personal", Phone: "+70000000000"},
			`{"next_step":"WhatsApp on the phone → Linked devices → Link a device → Link with phone number instead, ` +
				"enter the code before expires_at; then check manage-accounts action=list: " +
				"`linking` while it waits, `connected` once linked, `needs_link` with a reason if it failed" +
				`","pair_code":"ABCD-EFGH","expires_at":"` + expires.Local().Format(time.RFC3339) + `","status":"linking"}`},
		{"remove", &toolstest.WA{Removed: wa.RemoveResult{Hint: "remove the device on the phone manually"}},
			map[string]any{"action": "remove", "account_id": "personal"},
			toolstest.Call{Method: "Remove", Nick: "personal"},
			`{"hint":"remove the device on the phone manually","status":"removed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, text := call(t, connect(t, tc.fake), tc.args)
			if res.IsError || !sameJSON(t, text, tc.want) {
				t.Fatalf("isError=%v %s\nwant %s", res.IsError, text, tc.want)
			}
			if got := tc.fake.Calls(); !slices.Equal(got, []toolstest.Call{tc.call}) {
				t.Fatalf("WA calls = %+v, want %+v", got, tc.call)
			}
		})
	}
}

func TestManageAccountsAddRemoveReportErrors(t *testing.T) {
	fake := &toolstest.WA{Err: errors.New("linking failed: boom")}
	cs := connect(t, fake)
	res, text := call(t, cs, map[string]any{"action": "add", "account_id": "Bad Nick"})
	if !res.IsError || !strings.Contains(text, "invalid") {
		t.Fatalf("invalid id: isError=%v %s", res.IsError, text)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("invalid id reached WA: %v", calls)
	}
	for _, action := range []string{"add", "remove"} {
		res, text = call(t, cs, map[string]any{"action": action, "account_id": "personal"})
		if !res.IsError || !strings.Contains(text, "boom") {
			t.Fatalf("%s: isError=%v %s", action, res.IsError, text)
		}
	}
	want := []toolstest.Call{{Method: "Link", Nick: "personal"}, {Method: "Remove", Nick: "personal"}}
	if got := fake.Calls(); !slices.Equal(got, want) {
		t.Fatalf("WA calls = %+v, want %+v", got, want)
	}
}

func TestManageAccountsRejectsUnknownAction(t *testing.T) {
	res, _ := call(t, connect(t, &toolstest.WA{}), map[string]any{"action": "purge"})
	if !res.IsError {
		t.Fatal("unknown action accepted")
	}
}

func TestReadOnly(t *testing.T) {
	if ReadOnly("manage-accounts") || ReadOnly("no-such-tool") {
		t.Fatal("manage-accounts is not read-only")
	}
}
