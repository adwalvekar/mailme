package main

import (
	"fmt"
	"io"
	"runtime"
)

// Release builds set these with -ldflags. Source builds remain identifiable.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func printVersion(out io.Writer) error {
	_, err := fmt.Fprintf(out, "mailme %s (commit %s, built %s, %s/%s)\n", version, commit, buildDate, runtime.GOOS, runtime.GOARCH)
	return err
}
