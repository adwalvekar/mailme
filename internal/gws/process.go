package gws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"mailme/internal/gmail"
)

const maxOutputBytes = 64 << 20

type boundedOutput struct {
	buffer   bytes.Buffer
	overflow bool
	cancel   context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxOutputBytes-b.buffer.Len() {
		b.overflow = true
		b.cancel()
		return 0, errors.New("gws output exceeds limit")
	}
	return b.buffer.Write(p)
}

func (c *Client) invoke(ctx context.Context, args []string, out any) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := &boundedOutput{cancel: cancel}
	cmd := exec.CommandContext(runCtx, c.path, args...)
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard // CLI errors can contain message data or credentials.
	// Kill wrappers and their child process together on timeout/shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if stdout.overflow {
		return errMalformedResponse
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// API errors are JSON on stdout; preserve HTTP status for sync's expired
	// history / deleted-message handling, without returning Google's message.
	var envelope map[string]json.RawMessage
	decodeErr := json.Unmarshal(stdout.buffer.Bytes(), &envelope)
	if decodeErr == nil && envelope["error"] != nil {
		apiErr := gmail.ParseAPIError(stdout.buffer.Bytes(), 0)
		if apiErr.Status != 0 {
			return apiErr
		}
		return errors.New("gws reported an error; check CLI authentication and Gmail API permissions")
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("gws exited with code %d; check CLI authentication, Gmail permissions, and supported flags", exitErr.ExitCode())
		}
		return errors.New("cannot execute gws; check the configured executable path")
	}
	if decodeErr != nil || envelope == nil {
		return errMalformedResponse
	}
	if err := json.Unmarshal(stdout.buffer.Bytes(), out); err != nil {
		return errMalformedResponse
	}
	return nil
}
