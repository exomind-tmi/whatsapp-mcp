package proc

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

const detachFlags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP

// Spawn starts exe with no console, in its own process group and outside the
// caller's job object, so it survives the host's exit (Claude Desktop's MSIX
// job). `taskkill /T` follows recorded parents, so the shim reaches the daemon
// through a middle process that exits (shim.LaunchDaemon). If the job forbids
// breakaway, it retries without it: the child then dies with the host, which
// is acceptable.
func Spawn(exe string, args []string, dir string, stdout io.Writer) (*exec.Cmd, error) {
	cmd := command(exe, args, dir, stdout, detachFlags|windows.CREATE_BREAKAWAY_FROM_JOB)
	err := cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		cmd = command(exe, args, dir, stdout, detachFlags)
		err = cmd.Start()
	}
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

func command(exe string, args []string, dir string, stdout io.Writer, flags uint32) *exec.Cmd {
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	return cmd
}

// Alive reports whether a process with this pid is running (it need not be
// our child). A process we may not query is treated as alive.
func Alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}
