package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// Tail shows the active log's last lines and optionally follows its filename
// across rotation. Memory is bounded even for a very large pre-upgrade log.
func Tail(ctx context.Context, path string, out io.Writer, lines int, follow bool) error {
	if lines < 1 || lines > 10000 {
		return fmt.Errorf("--lines must be between 1 and 10000")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	start := max(int64(0), info.Size()-(8<<20))
	data := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil && err != io.EOF {
		return err
	}
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	position := end
	for n := 0; n < lines && position > 0; n++ {
		index := bytes.LastIndexByte(data[:position], '\n')
		if index < 0 {
			position = 0
			break
		}
		position = index
		if n == lines-1 {
			position++
			break
		}
	}
	if _, err := out.Write(data[position:]); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	if _, err := f.Seek(info.Size(), io.SeekStart); err != nil {
		return err
	}
	positionBytes := info.Size()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		// Drain the old descriptor before switching to the new active file.
		n, err := io.Copy(out, f)
		if err != nil {
			return err
		}
		positionBytes += n
		current, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		} // rotation's rename/create window
		if err != nil {
			return err
		}
		if !os.SameFile(info, current) {
			next, err := os.Open(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			f.Close()
			f = next
			info, err = f.Stat()
			if err != nil {
				return err
			}
			positionBytes = 0
		} else if current.Size() < positionBytes {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			positionBytes = 0
		}
	}
}
