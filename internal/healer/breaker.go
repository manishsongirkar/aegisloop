// Package healer implements the bounded self-healing loop: verify, build a
// minimal prompt, ask the model for edits, apply them in the sandbox, repeat.
package healer

// Halt reasons reported by the circuit breaker.
const (
	HaltMaxRetries  = "max_retries_exhausted"
	HaltNoProgress  = "no_progress"        // identical failures after a heal attempt
	HaltThrashing   = "thrashing_detected" // an earlier failure state came back
	HaltRegressing  = "regressing"         // failure count grew on consecutive attempts
	HaltUnusable    = "unusable_llm_reply" // model replies could not be applied twice in a row
	HaltInterrupted = "interrupted"
)

// Breaker is the loop's circuit breaker. It is pure state so it can be tested
// without a model, a sandbox or a clock.
type Breaker struct {
	MaxRetries int

	attempts    int
	seen        map[string]bool
	last        string
	lastCount   int
	growth      int
	unusableRun int
	haveLast    bool
}

// NewBreaker returns a breaker allowing max heal attempts.
func NewBreaker(max int) *Breaker {
	return &Breaker{MaxRetries: max, seen: map[string]bool{}}
}

// Attempts returns the number of heal attempts started.
func (b *Breaker) Attempts() int { return b.attempts }

// Observe records a failing verification state (fingerprint + number of
// failures). It returns a non-empty halt reason when the loop must stop.
func (b *Breaker) Observe(fingerprint string, failures int) string {
	defer func() {
		b.seen[fingerprint] = true
		b.last, b.lastCount, b.haveLast = fingerprint, failures, true
	}()
	if !b.haveLast {
		return ""
	}
	if fingerprint == b.last {
		return HaltNoProgress
	}
	if b.seen[fingerprint] {
		return HaltThrashing
	}
	if failures > b.lastCount {
		b.growth++
		if b.growth >= 2 {
			return HaltRegressing
		}
	} else {
		b.growth = 0
	}
	return ""
}

// Allow reports whether another heal attempt may start, and counts it.
func (b *Breaker) Allow() bool {
	if b.attempts >= b.MaxRetries {
		return false
	}
	b.attempts++
	return true
}

// Unusable records a model reply that produced no applicable edit. Two in a
// row halt the loop.
func (b *Breaker) Unusable() string {
	b.unusableRun++
	if b.unusableRun >= 2 {
		return HaltUnusable
	}
	return ""
}

// Usable resets the unusable-reply counter.
func (b *Breaker) Usable() { b.unusableRun = 0 }
