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

// ErrBinaryNotFound is returned by Locate when ptrack_py can be found
// neither next to the running ptrack binary nor in PATH.
var ErrBinaryNotFound = errors.New("ptrackpy: ptrack_py not found next to ptrack or in PATH")

// ErrIncompleteMeeting is returned by Run when ptrack_py rejects an
// input because it has no session_ended event (meeting still in
// progress). Wrapped with the file path detail in stderr.
var ErrIncompleteMeeting = errors.New("ptrackpy: meeting still in progress")

// incompleteMeetingExitCode mirrors INCOMPLETE_MEETING_EXIT_CODE in
// py/src/ptrack_py/__main__.py.
const incompleteMeetingExitCode = 3

// Locate returns the absolute path to the ptrack_py executable, checking
// first next to the running ptrack binary and then PATH. The returned
// path is cached after the first successful lookup.
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

// Run executes `ptrack_py <args...>` and returns its stdout bytes. On
// failure the captured stderr is folded into the returned error so the
// caller does not have to wire up its own diagnostic capture.
func Run(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := Locate()
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == incompleteMeetingExitCode {
			if msg != "" {
				return nil, fmt.Errorf("%w: %s", ErrIncompleteMeeting, msg)
			}
			return nil, ErrIncompleteMeeting
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

// argSummary keeps a short hint of which subcommand failed without
// dumping the full --in path list into every error.
func argSummary(args []string) string {
	if len(args) == 0 {
		return "ptrack_py"
	}
	return "ptrack_py " + args[0]
}
