package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxCapture bounds captured output per stream so a runaway tool cannot
// exhaust memory.
const maxCapture = 4 << 20

type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := maxCapture - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return c.buf.String() + "\n[output truncated]"
	}
	return c.buf.String()
}

// SplitCommand splits a command line into arguments honouring single and
// double quotes and backslash escapes (outside single quotes).
func SplitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	var quote rune
	started := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\'):
				i++
				cur.WriteRune(runes[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			started = true
		case r == ' ' || r == '\t' || r == '\n':
			if started || cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in %q", quote, s)
	}
	if started || cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args, nil
}

// LookTool finds an executable for name relative to dir: explicit relative
// paths first, then vendor/bin, node_modules/.bin and finally $PATH.
func LookTool(dir, name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, name)
		}
		if isExecutable(p) {
			return p, nil
		}
		return "", fmt.Errorf("%s not found", name)
	}
	for _, d := range []string{filepath.Join(dir, "vendor", "bin"), filepath.Join(dir, "node_modules", ".bin")} {
		if p := filepath.Join(d, name); isExecutable(p) {
			return p, nil
		}
		if runtime.GOOS == "windows" {
			for _, ext := range []string{".bat", ".cmd", ".exe"} {
				if p := filepath.Join(d, name+ext); isExecutable(p) {
					return p, nil
				}
			}
		}
	}
	return exec.LookPath(name)
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || st.Mode()&0o111 != 0
}

type execResult struct {
	Exit     int
	Stdout   string
	Stderr   string
	Duration time.Duration
	TimedOut bool
}

// execute runs argv (already resolved) in dir with a timeout.
func execute(ctx context.Context, dir string, argv []string, env []string, timeout time.Duration, shell bool) (execResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if shell {
		cmd = exec.CommandContext(cctx, "sh", "-c", strings.Join(argv, " "))
	} else {
		cmd = exec.CommandContext(cctx, argv[0], argv[1:]...)
	}
	cmd.Dir = dir
	cmd.Env = env
	var out, errb cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	setProcessGroup(cmd)
	start := time.Now()
	err := cmd.Run()
	res := execResult{Stdout: out.String(), Stderr: errb.String(), Duration: time.Since(start)}
	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut, res.Exit = true, -1
		return res, nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Exit = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

func buildEnv(dir string) []string {
	path := os.Getenv("PATH")
	extra := filepath.Join(dir, "vendor", "bin") + string(os.PathListSeparator) + filepath.Join(dir, "node_modules", ".bin")
	env := os.Environ()
	out := make([]string, 0, len(env)+4)
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") || strings.HasPrefix(e, "NO_COLOR=") || strings.HasPrefix(e, "FORCE_COLOR=") || strings.HasPrefix(e, "CI=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "PATH="+extra+string(os.PathListSeparator)+path, "NO_COLOR=1", "FORCE_COLOR=0", "CI=1")
}
