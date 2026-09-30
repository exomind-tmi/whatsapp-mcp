package shim

import (
	"fmt"
	"os"

	"github.com/exomind-tmi/whatsapp-mcp/internal/proc"
)

// LaunchDaemon is the middle process of a double spawn. It starts the daemon
// detached, prints its pid and exits at once, so the daemon's recorded parent
// is a dead process and `taskkill /T` on the shim cannot reach it. The shim
// watches the printed pid instead of waiting on a child.
func LaunchDaemon(dir string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-mcp: launch-daemon:", err)
		return 1
	}
	cmd, err := proc.Spawn(exe, []string{"daemon"}, dir, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-mcp: launch-daemon:", err)
		return 1
	}
	fmt.Println(cmd.Process.Pid)
	cmd.Process.Release()
	return 0
}
