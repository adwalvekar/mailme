// Package settings defines runtime tuning shared by the CLI and LaunchAgent.
package settings

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	"mailme/internal/gmail"
	"mailme/internal/logging"
)

type Runtime struct {
	SyncInterval         time.Duration
	LabelInterval        time.Duration
	SkippedRetryInterval time.Duration
	CheckpointInterval   time.Duration
	CheckpointBatchSize  int
	ReadConcurrency      int
	QuotaProfile         string
	Request              gmail.RequestPolicy
	Logs                 logging.Options
}

func Default() Runtime {
	return Runtime{
		SyncInterval: 5 * time.Second, LabelInterval: 5 * time.Minute,
		SkippedRetryInterval: 5 * time.Minute, CheckpointInterval: 5 * time.Second,
		CheckpointBatchSize: 250, ReadConcurrency: 6, QuotaProfile: "current",
		Request: gmail.DefaultRequestPolicy(), Logs: logging.Default(),
	}
}

func (r *Runtime) Flags(f *flag.FlagSet) {
	f.StringVar(&r.Logs.File, "log-file", r.Logs.File, "Persistent log file (default: ~/Library/Logs/mailme/bridge.log on macOS)")
	f.IntVar(&r.Logs.MaxSizeMB, "log-max-size", r.Logs.MaxSizeMB, "Rotate the active log at this many MiB (1–1024)")
	f.IntVar(&r.Logs.MaxBackups, "log-max-backups", r.Logs.MaxBackups, "Maximum rotated log files to retain (1–100)")
	f.IntVar(&r.Logs.MaxAgeDays, "log-max-age", r.Logs.MaxAgeDays, "Retain rotated logs for this many days (0 disables age expiry; count still bounded)")
	f.BoolVar(&r.Logs.Compress, "log-compress", r.Logs.Compress, "Compress rotated logs with gzip")
	f.StringVar(&r.Logs.Level, "log-level", r.Logs.Level, "Minimum log severity: debug, info, warn, error")
	f.StringVar(&r.Logs.Format, "log-format", r.Logs.Format, "Log format: text or json")
	f.BoolVar(&r.Logs.Stderr, "log-stderr", r.Logs.Stderr, "Also mirror application logs to stderr")
	f.DurationVar(&r.SyncInterval, "sync-interval", r.SyncInterval, "Gmail history polling interval (minimum 1s)")
	f.DurationVar(&r.LabelInterval, "label-interval", r.LabelInterval, "Folder definition refresh interval (minimum 1s)")
	f.DurationVar(&r.SkippedRetryInterval, "skipped-retry-interval", r.SkippedRetryInterval, "Retry interval for skipped metadata IDs (minimum 1s)")
	f.DurationVar(&r.CheckpointInterval, "checkpoint-interval", r.CheckpointInterval, "Publish/checkpoint metadata at least this often during backfill (minimum 1s)")
	f.IntVar(&r.CheckpointBatchSize, "checkpoint-batch-size", r.CheckpointBatchSize, "Metadata results per durable checkpoint (1–500)")
	f.IntVar(&r.ReadConcurrency, "read-concurrency", r.ReadConcurrency, "History/backfill metadata workers / gws read subprocesses (1–16)")
	f.StringVar(&r.QuotaProfile, "quota-profile", r.QuotaProfile, "Gmail quota profile: current or legacy (only if verified)")
	f.IntVar(&r.Request.QuotaUnitsPerSecond, "quota-units-per-second", r.Request.QuotaUnitsPerSecond, "Shared quota budget (0 uses profile default: current 80, legacy 125)")
	f.IntVar(&r.Request.ReadRetries, "read-retries", r.Request.ReadRetries, "Retries per read after transient API errors (0–10; writes never retry)")
	f.DurationVar(&r.Request.RetryMinDelay, "retry-min-delay", r.Request.RetryMinDelay, "Initial API/background sync retry delay, with jitter")
	f.DurationVar(&r.Request.RetryMaxDelay, "retry-max-delay", r.Request.RetryMaxDelay, "Maximum exponential retry delay; Retry-After may be longer")
	f.DurationVar(&r.Request.RequestTimeout, "request-timeout", r.Request.RequestTimeout, "Per HTTP request / gws subprocess deadline, excluding quota waits")
}

func (r Runtime) Validate() error {
	if err := r.Logs.Validate(); err != nil {
		return err
	}
	for _, item := range []struct {
		name  string
		value time.Duration
	}{
		{"sync-interval", r.SyncInterval}, {"label-interval", r.LabelInterval},
		{"skipped-retry-interval", r.SkippedRetryInterval}, {"checkpoint-interval", r.CheckpointInterval},
	} {
		if item.value < time.Second {
			return fmt.Errorf("--%s must be at least 1s", item.name)
		}
	}
	if r.ReadConcurrency < 1 || r.ReadConcurrency > 16 {
		return fmt.Errorf("--read-concurrency must be between 1 and 16")
	}
	if r.CheckpointBatchSize < 1 || r.CheckpointBatchSize > 500 {
		return fmt.Errorf("--checkpoint-batch-size must be between 1 and 500")
	}
	return r.Request.Validate(r.QuotaProfile)
}

// Args preserves every setting in a service definition, including explicit zero
// retries and a profile's automatic quota budget.
func (r Runtime) Args() []string {
	return []string{
		"--log-file", r.Logs.File, "--log-max-size", strconv.Itoa(r.Logs.MaxSizeMB),
		"--log-max-backups", strconv.Itoa(r.Logs.MaxBackups), "--log-max-age", strconv.Itoa(r.Logs.MaxAgeDays),
		"--log-compress=" + strconv.FormatBool(r.Logs.Compress), "--log-stderr=" + strconv.FormatBool(r.Logs.Stderr),
		"--log-level", r.Logs.Level, "--log-format", r.Logs.Format,
		"--sync-interval", r.SyncInterval.String(), "--label-interval", r.LabelInterval.String(),
		"--skipped-retry-interval", r.SkippedRetryInterval.String(), "--checkpoint-interval", r.CheckpointInterval.String(),
		"--checkpoint-batch-size", strconv.Itoa(r.CheckpointBatchSize), "--read-concurrency", strconv.Itoa(r.ReadConcurrency),
		"--quota-profile", r.QuotaProfile, "--quota-units-per-second", strconv.Itoa(r.Request.QuotaUnitsPerSecond),
		"--read-retries", strconv.Itoa(r.Request.ReadRetries), "--retry-min-delay", r.Request.RetryMinDelay.String(),
		"--retry-max-delay", r.Request.RetryMaxDelay.String(), "--request-timeout", r.Request.RequestTimeout.String(),
	}
}
