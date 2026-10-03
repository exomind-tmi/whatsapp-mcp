package client

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
)

// Status prints the daemon state and its accounts, always asking the daemon.
func Status(ctx context.Context, h home.Home, version string, w io.Writer) error {
	info, err := Probe(ctx, h)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "daemon %s  pid %d  port %d  up since %s\nhome   %s\n\n",
		info.Version, info.PID, info.Port, info.StartedAt.Local().Format(time.DateTime), h.Dir)

	sess, err := Connect(ctx, h, info.Port, version)
	if err != nil {
		return err
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "manage-accounts", Arguments: map[string]any{"action": "list"}})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("manage-accounts list: %s", cmp.Or(tools.ResultText(res), "(no text)"))
	}
	var list tools.ManageOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("decode manage-accounts list: %w", err)
	}
	if len(list.Accounts) == 0 {
		fmt.Fprintln(w, "no accounts linked")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tSTATUS\tPHONE\tCHATS\tMESSAGES\tREASON")
	for _, a := range list.Accounts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", a.AccountID, a.Status, a.Phone, a.Chats, a.Messages, a.Reason)
	}
	return tw.Flush()
}
