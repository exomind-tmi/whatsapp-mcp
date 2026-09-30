// Command whatsapp-mcp is both the stdio MCP shim a Claude host launches and
// the per-machine daemon it forwards to.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
	"github.com/exomind-tmi/whatsapp-mcp/internal/daemon"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/shim"
)

// version is set by scripts/build.sh via -ldflags "-X main.version=…".
var version = "v0.0.0-dev"

const usage = `usage: whatsapp-mcp <command>

  stdio    MCP server on stdin/stdout (what Claude launches)
  daemon   run the daemon in the foreground
  status   show the daemon and its accounts
  stop     stop the daemon
  version  print the version`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if args[0] == "version" {
		fmt.Println(version)
		return 0
	}
	h, err := home.Resolve()
	if err == nil {
		err = h.Ensure()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-mcp:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch args[0] {
	case "stdio":
		err = shim.Run(ctx, h, version, &mcp.StdioTransport{})
	case "daemon":
		err = daemon.Run(ctx, h, version)
	case "launch-daemon": // internal: middle process of the shim's double spawn
		return shim.LaunchDaemon(h.Dir)
	case "status":
		err = client.Status(ctx, h, version, os.Stdout)
	case "stop":
		err = client.Stop(ctx, h)
		if errors.Is(err, client.ErrNotRunning) {
			fmt.Println("daemon is not running")
			return 0
		}
		if err == nil {
			fmt.Println("daemon stopped")
		}
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if errors.Is(err, client.ErrNotRunning) {
		fmt.Println("daemon is not running")
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-mcp:", err)
		return 1
	}
	return 0
}
