package tokenizer

import (
	"fmt"
	"regexp"
	"strings"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// StripANSI removes terminal colour/control sequences.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// StripOptions tunes StripNoise.
type StripOptions struct {
	Root     string // absolute project root; trimmed from paths
	MaxLines int    // hard cap on output lines (default 60)
	MaxChars int    // hard cap on output bytes (default 6000)
}

// Stripped is the result of StripNoise.
type Stripped struct {
	Text           string
	OriginalBytes  int
	StrippedBytes  int
	DroppedLines   int
	DroppedFrames  int
	CollapsedLines int
}

// ReductionPercent reports how much smaller the stripped text is.
func (s Stripped) ReductionPercent() float64 {
	if s.OriginalBytes == 0 {
		return 0
	}
	return 100 * (1 - float64(s.StrippedBytes)/float64(s.OriginalBytes))
}

var noiseLine = []*regexp.Regexp{
	// vendor / dependency / runtime frames
	regexp.MustCompile(`(?:^|[\s/\\(])vendor[/\\]`),
	regexp.MustCompile(`(?:^|[\s/\\(])node_modules[/\\]`),
	regexp.MustCompile(`node:internal|internal/(?:modules|process|timers)|\(native\)|\(<anonymous>\)|\bat <anonymous>|processTicksAndRejections`),
	regexp.MustCompile(`^\s*(?:goroutine \d+ \[|created by |runtime\.|testing\.tRunner|/usr/local/go/src/|.*GOROOT)`),
	regexp.MustCompile(`^\s*(?:#\d+\s+\{main\}|\[internal function\])`),
	// PHPUnit / composer / npm / jest banners
	regexp.MustCompile(`^(?:PHPUnit \d|Runtime:|Configuration:|Random Seed:|Time: [\d:.]+, Memory:|Generating code coverage|Warning: .*xdebug|Sebastian Bergmann)`),
	regexp.MustCompile(`^[.FEWSIRD]+(?:\s+\d+ / \d+\s+\(\s*\d+%\))?$`),
	regexp.MustCompile(`^(?:PASS|RUNS)\s+\S+`),
	regexp.MustCompile(`^(?:npm (?:WARN|notice|timing|http|info)|npm ERR! (?:code|errno|path|syscall|A complete log|This is probably)|> .* (?:test|build|lint)$|Test Suites: .*passed)`),
	regexp.MustCompile(`^(?:Deprecated: .*composer|Do not run Composer as root|\s*Fixed\s+\d+ of|Done in [\d.]+s)`),
	regexp.MustCompile(`^\s*(?:at .* \(node:|at new Promise|at Generator\.|at async )`),
	regexp.MustCompile(`^\s*\^+\s*$`),
}

// StripNoise removes framework boilerplate from raw stderr/stdout while keeping
// the high-signal lines (assertion messages, linter findings, error locations).
func StripNoise(raw string, opts StripOptions) Stripped {
	if opts.MaxLines <= 0 {
		opts.MaxLines = 60
	}
	if opts.MaxChars <= 0 {
		opts.MaxChars = 6000
	}
	res := Stripped{OriginalBytes: len(raw)}
	text := StripANSI(strings.ReplaceAll(raw, "\r\n", "\n"))
	if opts.Root != "" {
		text = strings.ReplaceAll(text, strings.TrimRight(opts.Root, "/")+"/", "")
	}

	var kept []string
	pendingFrames := 0
	flush := func() {
		if pendingFrames > 0 {
			kept = append(kept, fmt.Sprintf("    … %d framework/vendor frame(s) omitted", pendingFrames))
			res.DroppedFrames += pendingFrames
			pendingFrames = 0
		}
	}
	prevBlank := true
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			if !prevBlank {
				flush()
				kept = append(kept, "")
			}
			prevBlank = true
			continue
		}
		prevBlank = false
		if isNoise(line) {
			if looksLikeFrame(line) {
				pendingFrames++
			} else {
				res.DroppedLines++
			}
			continue
		}
		flush()
		kept = append(kept, line)
	}
	flush()

	// collapse consecutive duplicates
	var dedup []string
	for i := 0; i < len(kept); {
		j := i
		for j+1 < len(kept) && kept[j+1] == kept[i] && kept[i] != "" {
			j++
		}
		if j > i {
			dedup = append(dedup, fmt.Sprintf("%s  (×%d)", kept[i], j-i+1))
			res.CollapsedLines += j - i
		} else {
			dedup = append(dedup, kept[i])
		}
		i = j + 1
	}
	for len(dedup) > 0 && dedup[len(dedup)-1] == "" {
		dedup = dedup[:len(dedup)-1]
	}
	for len(dedup) > 0 && dedup[0] == "" {
		dedup = dedup[1:]
	}

	if len(dedup) > opts.MaxLines {
		head := opts.MaxLines * 2 / 3
		tail := opts.MaxLines - head
		omitted := len(dedup) - head - tail
		cut := append([]string{}, dedup[:head]...)
		cut = append(cut, fmt.Sprintf("… %d line(s) omitted …", omitted))
		dedup = append(cut, dedup[len(dedup)-tail:]...)
	}
	out := strings.Join(dedup, "\n")
	if len(out) > opts.MaxChars {
		h := opts.MaxChars * 2 / 3
		t := opts.MaxChars - h
		out = out[:h] + "\n… output truncated …\n" + out[len(out)-t:]
	}
	res.Text = out
	res.StrippedBytes = len(out)
	return res
}

func isNoise(line string) bool {
	for _, re := range noiseLine {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

var frameRe = regexp.MustCompile(`^\s*(?:#\d+\s|at\s|\S+\(\)\s*$|\S+:\d+\s*$|/.*\.go:\d+)`)

func looksLikeFrame(line string) bool { return frameRe.MatchString(line) }
