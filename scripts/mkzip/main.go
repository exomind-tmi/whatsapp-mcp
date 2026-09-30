// Command mkzip zips a directory the way Claude Desktop accepts plugins:
// entry names relative to the directory, without a "./" prefix, and Unix
// modes so the Linux binary keeps its exec bit. No zip tool is needed.
//
//	go run ./scripts/mkzip -x server/whatsapp-mcp out.zip dir
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	var execs multi
	flag.Var(&execs, "x", "entry to mark executable (0755); repeatable")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkzip [-x entry]... out.zip dir")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), flag.Arg(1), execs); err != nil {
		fmt.Fprintln(os.Stderr, "mkzip:", err)
		os.Exit(1)
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func run(out, dir string, execs []string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	marked := map[string]bool{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		mode := fs.FileMode(0o644)
		for _, x := range execs {
			if x == name {
				mode = 0o755
				marked[name] = true
			}
		}
		return add(zw, p, name, mode)
	})
	if err == nil {
		for _, x := range execs {
			if !marked[x] {
				err = fmt.Errorf("executable entry %q not found in %s", x, dir)
			}
		}
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func add(zw *zip.Writer, path, name string, mode fs.FileMode) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: st.ModTime()}
	hdr.SetMode(mode)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(w, src)
	return err
}
