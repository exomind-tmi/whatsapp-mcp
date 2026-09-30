package home

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
)

// EnvLogLevel selects the log level: debug, info (default), warn, error.
const EnvLogLevel = "WHATSAPP_MCP_LOG"

const rotateSize = 10 << 20

// RotateLog moves the log to .1 when it grew past 10 MB. Failures are
// ignored: on Windows another process holding the file open blocks the
// rename, which is fine. On Linux the rename succeeds under a live writer,
// so only the file's owner (the lock-holding daemon) may call it.
func RotateLog(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() > rotateSize {
		_ = os.Rename(path, path+".1")
	}
}

// OpenLog opens an append-only slog file logger.
func OpenLog(path string) (*slog.Logger, *os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	h := slog.NewTextHandler(f, &slog.HandlerOptions{Level: logLevel()})
	return slog.New(h).With("pid", os.Getpid()), f, nil
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv(EnvLogLevel))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Tail returns up to n last lines of the file at path.
func Tail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const window = 64 << 10
	if st, err := f.Stat(); err == nil && st.Size() > window {
		_, _ = f.Seek(-window, io.SeekEnd)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(bytes.TrimRight(b, "\r\n")), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	return lines
}
