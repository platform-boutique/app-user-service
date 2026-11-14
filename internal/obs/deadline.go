package obs

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

// DefaultRequestTimeout is the longest a unary call may run on the server (CODE-05).
const DefaultRequestTimeout = 5 * time.Second

// DeadlineUnaryInterceptor bounds every unary call to d: a call without a deadline gets one, and a caller deadline
// that is later than d is shortened to d. The context reaches the handlers and with it every database call, so no
// query can wait longer than d, whatever the client did. d <= 0 switches the bound off. A shorter deadline of the
// caller is never extended. Put it before the handlers and after the tracing interceptor.
func DeadlineUnaryInterceptor(d time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if d <= 0 {
			return handler(ctx, req)
		}
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > d {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return handler(ctx, req)
	}
}

// RequestTimeoutFromEnv reads REQUEST_TIMEOUT (a Go duration such as "5s"; "0" switches the bound off). A value that
// does not parse, is negative or is below 100 ms keeps the default and is returned as bad so the caller can warn.
func RequestTimeoutFromEnv(get Getenv) (d time.Duration, bad string) {
	v := get("REQUEST_TIMEOUT")
	if v == "" {
		return DefaultRequestTimeout, ""
	}
	p, err := time.ParseDuration(v)
	if err != nil || p < 0 || (p > 0 && p < 100*time.Millisecond) {
		return DefaultRequestTimeout, v
	}
	return p, ""
}
