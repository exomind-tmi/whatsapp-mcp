package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return res, sb.String()
}

func TestServerAdvertisesInstructionsAndPinnedProtocol(t *testing.T) {
	cs := connect(t, wa.NewManager())
	init := cs.InitializeResult()
	if init.Instructions != Instructions {
		t.Error("instructions not advertised")
	}
	if init.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol = %s, want %s", init.ProtocolVersion, ProtocolVersion)
	}
}

func TestManageAccountsSchema(t *testing.T) {
	cs := connect(t, wa.NewManager())
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "manage-accounts" {
		t.Fatalf("tools = %+v", list.Tools)
	}
	tool := list.Tools[0]
	if tool.Description != manageAccountsDescription || !strings.Contains(tool.Description, "PERMANENTLY DELETES") {
		t.Error("description differs from plan 5.3")
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
	list, err := connect(t, wa.NewManager()).ListTools(context.Background(), nil)
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
	list, err := connect(t, wa.NewManager()).ListTools(context.Background(), nil)
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
	res, text := call(t, connect(t, wa.NewManager()), map[string]any{"action": "list"})
	if res.IsError {
		t.Fatalf("error: %s", text)
	}
	if !strings.Contains(text, `"accounts":[]`) || !strings.Contains(text, "action=add") {
		t.Fatalf("list = %s", text)
	}
}

func TestManageAccountsAddValidatesThenReportsM1(t *testing.T) {
	cs := connect(t, wa.NewManager())
	res, text := call(t, cs, map[string]any{"action": "add", "account_id": "Bad Nick"})
	if !res.IsError || !strings.Contains(text, "invalid") {
		t.Fatalf("invalid id: isError=%v %s", res.IsError, text)
	}
	for _, action := range []string{"add", "remove"} {
		res, text = call(t, cs, map[string]any{"action": action, "account_id": "personal"})
		if !res.IsError || !strings.Contains(text, "not implemented until M1") {
			t.Fatalf("%s: isError=%v %s", action, res.IsError, text)
		}
	}
}

func TestManageAccountsRejectsUnknownAction(t *testing.T) {
	res, _ := call(t, connect(t, wa.NewManager()), map[string]any{"action": "purge"})
	if !res.IsError {
		t.Fatal("unknown action accepted")
	}
}

func TestReadOnly(t *testing.T) {
	if ReadOnly("manage-accounts") || ReadOnly("no-such-tool") {
		t.Fatal("manage-accounts is not read-only")
	}
}
