package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestProcessStreamsAndExitStatus exercises the real main function in a child
// process. A returned command error must become stderr plus exit 1, while a
// machine-readable success keeps stderr empty.
func TestProcessStreamsAndExitStatus(t *testing.T) {
	if mode := os.Getenv("AIUSAGE_PROCESS_CONTRACT"); mode != "" {
		switch mode {
		case "version":
			os.Args = []string{"aiusage", "version"}
		case "invalid-format":
			os.Args = []string{"aiusage", "export", "--format", "yaml"}
		default:
			panic("unknown process contract mode: " + mode)
		}
		main()
		return
	}

	tests := []struct {
		name       string
		mode       string
		wantExit   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "machine success uses stdout",
			mode:       "version",
			wantExit:   0,
			wantStdout: "dev-",
		},
		{
			name:       "command error uses stderr and exit one",
			mode:       "invalid-format",
			wantExit:   1,
			wantStderr: `invalid --format "yaml": want json or csv`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestProcessStreamsAndExitStatus$")
			command.Env = append(os.Environ(), "AIUSAGE_PROCESS_CONTRACT="+tc.mode)
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr

			err := command.Run()
			exit := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatalf("run child: %v", err)
				}
				exit = exitError.ExitCode()
			}
			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d; stdout=%q stderr=%q",
					exit, tc.wantExit, stdout.String(), stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout = %q, want text %q", stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want text %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantExit == 0 && stderr.Len() != 0 {
				t.Errorf("successful machine command wrote stderr: %q", stderr.String())
			}
			if tc.wantExit != 0 && stdout.Len() != 0 {
				t.Errorf("failed command wrote stdout: %q", stdout.String())
			}
		})
	}
}
