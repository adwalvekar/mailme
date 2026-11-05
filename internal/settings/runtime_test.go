package settings

import (
	"flag"
	"reflect"
	"testing"
)

func TestRuntimeSettingsRoundTripAndValidation(t *testing.T) {
	want := Default()
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	want.Flags(f)
	if err := f.Parse([]string{
		"--sync-interval", "1s", "--label-interval", "2m", "--skipped-retry-interval", "10m",
		"--checkpoint-interval", "2s", "--checkpoint-batch-size", "80", "--read-concurrency", "12",
		"--quota-profile", "legacy", "--quota-units-per-second", "200", "--read-retries", "0",
		"--retry-min-delay", "2s", "--retry-max-delay", "30s", "--request-timeout", "2m",
	}); err != nil {
		t.Fatal(err)
	}
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
	got := Default()
	restored := flag.NewFlagSet("serve", flag.ContinueOnError)
	got.Flags(restored)
	if err := restored.Parse(want.Args()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("settings lost in service round trip: %+v != %+v", want, got)
	}
	for _, args := range [][]string{
		{"--sync-interval", "0s"}, {"--sync-interval", "999ms"}, {"--label-interval", "-1s"},
		{"--skipped-retry-interval", "0s"}, {"--checkpoint-interval", "0s"},
		{"--read-concurrency", "0"}, {"--checkpoint-batch-size", "501"},
		{"--quota-profile", "typo"}, {"--quota-units-per-second", "101"}, {"--quota-units-per-second", "-1"},
		{"--read-retries", "-1"}, {"--retry-min-delay", "2m"}, {"--request-timeout", "0s"},
	} {
		r := Default()
		f := flag.NewFlagSet("serve", flag.ContinueOnError)
		r.Flags(f)
		if err := f.Parse(args); err != nil {
			t.Fatal(err)
		}
		if r.Validate() == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
}
