// Package logging owns bounded, private application logs independently of stdio.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"

	"gopkg.in/natefinch/lumberjack.v2"
)

type Options struct {
	File       string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
	Level      string
	Format     string
	Stderr     bool
}

func Default() Options {
	return Options{MaxSizeMB: 10, MaxBackups: 5, MaxAgeDays: 7, Compress: true, Level: "info", Format: "text", Stderr: true}
}

func DefaultPath() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Logs", "mailme", "bridge.log"), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mailme", "bridge.log"), nil
}

func (o Options) Validate() error {
	if o.File != "" && !filepath.IsAbs(o.File) {
		return errors.New("--log-file must be an absolute path")
	}
	if o.MaxSizeMB < 1 || o.MaxSizeMB > 1024 {
		return errors.New("--log-max-size must be between 1 and 1024 MiB")
	}
	if o.MaxBackups < 1 || o.MaxBackups > 100 {
		return errors.New("--log-max-backups must be between 1 and 100")
	}
	if o.MaxAgeDays < 0 || o.MaxAgeDays > 365 {
		return errors.New("--log-max-age must be between 0 (count limit only) and 365 days")
	}
	if o.Level != "debug" && o.Level != "info" && o.Level != "warn" && o.Level != "error" {
		return errors.New("--log-level must be debug, info, warn, or error")
	}
	if o.Format != "text" && o.Format != "json" {
		return errors.New("--log-format must be text or json")
	}
	return nil
}

type Session struct {
	Logger   *slog.Logger
	writer   *lumberjack.Logger
	lock     *os.File
	once     sync.Once
	closeErr error
}

func Open(o Options, stderr io.Writer) (*Session, error) {
	if o.File == "" {
		var err error
		o.File, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(o.File), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(o.File+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("log file is already in use; choose a separate --log-file for another bridge: %w", err)
	}
	s := &Session{lock: lock}
	f, err := os.OpenFile(o.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err == nil {
		var info os.FileInfo
		info, err = f.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("log path must be a regular file")
		}
		if err == nil {
			err = f.Chmod(0600)
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	s.writer = &lumberjack.Logger{Filename: o.File, MaxSize: o.MaxSizeMB, MaxBackups: o.MaxBackups, MaxAge: o.MaxAgeDays, Compress: o.Compress}
	// Open now so a headless service reports permission failures before serving.
	// This also rotates an oversized log left by an older installation.
	if _, err := s.writer.Write(nil); err != nil {
		s.Close()
		return nil, err
	}
	var level slog.Level
	_ = level.UnmarshalText([]byte(o.Level))
	writer := &output{file: s.writer, stderr: stderr, mirror: o.Stderr}
	handlerOptions := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(writer, handlerOptions)
	if o.Format == "json" {
		handler = slog.NewJSONHandler(writer, handlerOptions)
	}
	s.Logger = slog.New(handler)
	return s, nil
}

func (s *Session) Close() error {
	s.once.Do(func() {
		if s.writer != nil {
			s.closeErr = s.writer.Close()
		}
		if s.lock != nil {
			err := s.lock.Close() // closing releases the advisory lock; keep its inode
			if s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

type output struct {
	file   io.Writer
	stderr io.Writer
	mirror bool
}

func (w *output) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if w.stderr != nil {
		if w.mirror {
			_, _ = w.stderr.Write(p)
		}
		if err != nil {
			_, _ = fmt.Fprintf(w.stderr, "mailme: cannot write application log: %v\n", err)
		}
	}
	return n, err
}
