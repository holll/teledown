package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestInvalidCommandArgumentsFailBeforeConnecting(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{"info positional", runInfoCommand, []string{"extra"}, "位置参数"},
		{"download missing ID", runDownloadCommand, []string{"-id", "  "}, "必须指定 -id"},
		{"download threads", runDownloadCommand, []string{"-id", "12", "-threads", "0"}, "-threads"},
		{"upload threads", runUploadCommand, []string{"-path", "file", "-threads", "-1"}, "-threads"},
		{"upload delete switch", runUploadCommand, []string{"-path", "file", "-dau", "typo"}, "-dau"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestSubcommandHelpReturnsFlagErrHelp(t *testing.T) {
	for _, run := range []func([]string) error{runInfoCommand, runDownloadCommand, runUploadCommand} {
		if err := run([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("error = %v; want flag.ErrHelp", err)
		}
	}
}
