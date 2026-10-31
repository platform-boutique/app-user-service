package users

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults of the limiters.
const (
	DefaultLoginLimit  = 5
	DefaultLoginWindow = time.Minute
	// DefaultRegisterLimit is a flood brake for the whole process (the frontend is the only caller, so
	// there is no client address to key on), not a per-client limit.
	DefaultRegisterLimit  = 60
	DefaultRegisterWindow = time.Minute
	// maxLimiterKeys bounds the limiter's memory: with this many distinct keys that are all still inside
	// their window, a new key is refused (fail closed) until some expire.
	maxLimiterKeys = 50_000
	// maxKeyLen keeps one key small; longer keys are cut (an e-mail address has at most 254 bytes).
	maxKeyLen = 254
)

// RateLimiter is a sliding-window limiter: at most limit attempts per key in any window. It lives in the
// memory of one process, so with N replicas the effective limit is N times higher, and a restart resets it.
// A limit <= 0 disables it.
//
// For logins the attempt is recorded BEFORE the password is checked (so parallel guesses cannot all slip
// through while bcrypt runs) and a successful login calls Reset. Refused attempts are not recorded, so a
// block never lasts longer than one window after the last counted attempt: someone who keeps failing for
// an address cannot lock its owner out for good.
type RateLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu     sync.Mutex
	events map[string][]time.Time
}

// NewRateLimiter creates a limiter; now is the clock (nil = time.Now).
func NewRateLimiter(limit int, window time.Duration, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{limit: limit, window: window, now: now, events: map[string][]time.Time{}}
}

// Enabled reports whether the limiter limits anything.
func (l *RateLimiter) Enabled() bool { return l != nil && l.limit > 0 && l.window > 0 }

func clipKey(key string) string {
	if len(key) > maxKeyLen {
		return key[:maxKeyLen]
	}
	return key
}

// Allow records an attempt for key and reports whether it is within the limit. When it is not, retryAfter
// says when the oldest attempt leaves the window. Rejected attempts are not recorded.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if !l.Enabled() {
		return true, 0
	}
	key = clipKey(key)
	now := l.now()
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := l.events[key]
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.limit {
		l.events[key] = ts
		return false, ts[0].Add(l.window).Sub(now)
	}
	if len(ts) == 0 && len(l.events) >= maxLimiterKeys {
		l.sweep(cutoff)
		if len(l.events) >= maxLimiterKeys {
			return false, l.window
		}
	}
	l.events[key] = append(ts, now)
	return true, 0
}

// Reset forgets the attempts of key (called after a successful login).
func (l *RateLimiter) Reset(key string) {
	if !l.Enabled() {
		return
	}
	l.mu.Lock()
	delete(l.events, clipKey(key))
	l.mu.Unlock()
}

// sweep drops keys whose events are all outside the window. Caller holds mu.
func (l *RateLimiter) sweep(cutoff time.Time) {
	for k, ts := range l.events {
		if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
			delete(l.events, k)
		}
	}
}

// RateLimitFromEnv reads <prefix>_RATE_LIMIT (attempts per window, 0 disables) and <prefix>_RATE_WINDOW (a
// Go duration such as 30s or 2m) with the given defaults. A value that cannot be parsed yields the default
// for that setting and an error describing it, so a typo never silently switches the protection off.
func RateLimitFromEnv(getenv func(string) string, prefix string, defLimit int, defWindow time.Duration) (limit int, window time.Duration, err error) {
	limit, window = defLimit, defWindow
	var problems []string
	if name := prefix + "_RATE_LIMIT"; strings.TrimSpace(getenv(name)) != "" {
		v := strings.TrimSpace(getenv(name))
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			problems = append(problems, fmt.Sprintf("%s=%q is not a non-negative integer, using %d", name, v, defLimit))
		} else {
			limit = n
		}
	}
	if name := prefix + "_RATE_WINDOW"; strings.TrimSpace(getenv(name)) != "" {
		v := strings.TrimSpace(getenv(name))
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			problems = append(problems, fmt.Sprintf("%s=%q is not a positive duration, using %s", name, v, defWindow))
		} else {
			window = d
		}
	}
	if len(problems) > 0 {
		err = fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return limit, window, err
}
