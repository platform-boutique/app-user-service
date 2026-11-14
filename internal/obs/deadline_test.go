package obs

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// run calls the interceptor and returns the deadline the handler saw.
func run(t *testing.T, ctx context.Context, d time.Duration) (time.Duration, bool) {
	t.Helper()
	var left time.Duration
	var has bool
	h := func(ctx context.Context, req any) (any, error) {
		var dl time.Time
		if dl, has = ctx.Deadline(); has {
			left = time.Until(dl)
		}
		return "ok", nil
	}
	if _, err := DeadlineUnaryInterceptor(d)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"}, h); err != nil {
		t.Fatal(err)
	}
	return left, has
}

func TestDeadlineIsAddedWhenTheCallerSetsNone(t *testing.T) {
	left, has := run(t, context.Background(), time.Hour)
	if !has || left <= 59*time.Minute || left > time.Hour {
		t.Fatalf("deadline in %v (has=%v), want about 1h", left, has)
	}
}

func TestDeadlineOfTheCallerIsKeptWhenShorter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	left, has := run(t, ctx, time.Hour)
	if !has || left > time.Minute {
		t.Fatalf("deadline in %v, want at most the caller's 1m", left)
	}
}

func TestDeadlineOfTheCallerIsShortenedWhenLonger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	left, has := run(t, ctx, time.Hour)
	if !has || left > time.Hour {
		t.Fatalf("deadline in %v, want at most 1h", left)
	}
}

func TestDeadlineOffWithZero(t *testing.T) {
	if _, has := run(t, context.Background(), 0); has {
		t.Fatal("a deadline was added although the bound is off")
	}
}

func TestDeadlineContextEndsWithTheHandler(t *testing.T) {
	var seen context.Context
	h := func(ctx context.Context, req any) (any, error) { seen = ctx; return nil, nil }
	if _, err := DeadlineUnaryInterceptor(time.Hour)(context.Background(), nil, &grpc.UnaryServerInfo{}, h); err != nil {
		t.Fatal(err)
	}
	if seen.Err() == nil {
		t.Fatal("the timer context was not cancelled after the call (timer leak)")
	}
}

func TestRequestTimeoutFromEnv(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    time.Duration
		wantBad string
	}{
		{"", DefaultRequestTimeout, ""},
		{"2s", 2 * time.Second, ""},
		{"0", 0, ""},
		{"250ms", 250 * time.Millisecond, ""},
		{"soon", DefaultRequestTimeout, "soon"},
		{"-1s", DefaultRequestTimeout, "-1s"},
		{"10ms", DefaultRequestTimeout, "10ms"},
	} {
		got, bad := RequestTimeoutFromEnv(func(string) string { return c.in })
		if got != c.want || bad != c.wantBad {
			t.Errorf("REQUEST_TIMEOUT=%q: got (%v, %q), want (%v, %q)", c.in, got, bad, c.want, c.wantBad)
		}
	}
}
