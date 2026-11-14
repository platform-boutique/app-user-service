package users

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"user-service/internal/obs"
	pb "user-service/proto"

	"go.opentelemetry.io/otel/trace"
)

func spanCtx() context.Context {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	c := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	return obs.WithRequestID(c, "req-3")
}

func jsonLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

// OBS-30: the request path logs in the contract's shape with the request's trace ids, and no line
// shows an e-mail address, a password or a token.
func TestRequestPathLogsCarryTraceIDsAndNoSecrets(t *testing.T) {
	e := newTestService(t)
	e.svc.log.SetFormatter(obs.NewFormatter("user-service", "test"))
	const pw = "S3cret-p4ssw0rd-MARKER"
	c := spanCtx()
	reg, err := e.svc.Register(c, &pb.RegisterRequest{Email: "marker.user@example.com", Password: pw, Name: "Marker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(c, &pb.LoginRequest{Email: "marker.user@example.com", Password: pw}); err != nil {
		t.Fatal(err)
	}
	_, _ = e.svc.Login(c, &pb.LoginRequest{Email: "marker.user@example.com", Password: "wrong-" + pw})
	e.store.setErr(fmt.Errorf("%w: connection refused", ErrUnavailable))
	_, _ = e.svc.GetSession(c, &pb.GetSessionRequest{Token: reg.Token})
	e.svc.Wait()

	lines := jsonLines(t, e.logs)
	if len(lines) < 4 {
		t.Fatalf("expected request logs, got:\n%s", e.logs.String())
	}
	sawUnavailable := false
	for _, m := range lines {
		if m["message"] == "expired-session clean-up failed" || m["message"] == "deleted 1 expired session(s)" {
			continue // background work has no request, hence no ids
		}
		if m["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || m["span_id"] != "00f067aa0ba902b7" || m["request_id"] != "req-3" {
			t.Errorf("line without the request's ids: %v", m)
		}
		if m["service"] != "user-service" || m["severity"] == nil || m["timestamp"] == nil || m["message"] == nil {
			t.Errorf("shape: %v", m)
		}
		if m["severity"] == "warning" && m["error"] != nil {
			sawUnavailable = true
		}
	}
	if !sawUnavailable {
		t.Errorf("database outage not logged with an error field:\n%s", e.logs.String())
	}
	out := e.logs.String()
	for _, secret := range []string{"marker.user@example.com", pw, reg.Token} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q in the logs", secret)
		}
	}
	if lines[0]["user_id"] == nil {
		t.Errorf("registration line without user_id: %v", lines[0])
	}
}

// Outside a span the ids are absent.
func TestLogsWithoutSpanHaveNoIDs(t *testing.T) {
	e := newTestService(t)
	e.svc.log.SetFormatter(obs.NewFormatter("user-service", "test"))
	_ = register(t, e, "a@example.com", "Pa55word-ok-123")
	for _, m := range jsonLines(t, e.logs) {
		for _, k := range []string{"trace_id", "span_id", "request_id"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s present outside a span: %v", k, m)
			}
		}
	}
}
