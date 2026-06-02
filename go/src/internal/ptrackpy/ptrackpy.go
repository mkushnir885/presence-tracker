package ptrackpy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var cachedPath atomic.Pointer[string]

var ErrBinaryNotFound = errors.New("ptrackpy: ptrack_py not found next to ptrack or in PATH")

var ErrInvalidData = errors.New("ptrackpy: invalid meeting data")

const invalidDataExitCode = 3

func Locate() (string, error) {
	if p := cachedPath.Load(); p != nil {
		return *p, nil
	}
	path, err := locate()
	if err != nil {
		return "", err
	}
	cachedPath.Store(&path)
	return path, nil
}

// Run invokes the ptrack_py binary and returns its stdout. Exit code 3 — the
// Python side's "invalid data" signal (e.g. an events file with no
// session_ended event) — is mapped to ErrInvalidData so callers (the GUI)
// can show a 409 instead of failing.
func Run(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := Locate()
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is the resolved ptrack_py path; args are fixed subcommands and validated paths
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == invalidDataExitCode {
			if msg != "" {
				return nil, fmt.Errorf("%w: %s", ErrInvalidData, msg)
			}
			return nil, ErrInvalidData
		}
		if msg != "" {
			return nil, fmt.Errorf("ptrackpy: %s: %w: %s", argSummary(args), err, msg)
		}
		return nil, fmt.Errorf("ptrackpy: %s: %w", argSummary(args), err)
	}

	slog.Debug("ptrackpy: ran subcommand", "args", args, "bytes", stdout.Len())
	return stdout.Bytes(), nil
}

func locate() (string, error) {
	self, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(self)
		for _, name := range []string{"ptrack_py", "ptrack_py.exe"} {
			if candidate := filepath.Join(dir, name); fileExists(candidate) {
				return candidate, nil
			}
		}
	}
	if path, err := exec.LookPath("ptrack_py"); err == nil {
		return path, nil
	}
	return "", ErrBinaryNotFound
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func argSummary(args []string) string {
	if len(args) == 0 {
		return "ptrack_py"
	}
	return "ptrack_py " + args[0]
}
