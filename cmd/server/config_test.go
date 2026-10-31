package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func clearTelemetryEnv(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "COLLECTOR_SERVICE_ADDR", "OTEL_SDK_DISABLED", "ENABLE_TRACING", "PYROSCOPE_ADDR", "ENABLE_PROFILING"} {
		t.Setenv(k, "")
	}
}

func TestTelemetryOffByDefault(t *testing.T) {
	clearTelemetryEnv(t)
	if ep := otlpEndpoint(); ep != "" {
		t.Fatalf("endpoint %q with everything unset", ep)
	}
	shutdown, err := initTelemetry(context.Background(), serviceName, log)
	if !errors.Is(err, errTelemetryOff) {
		t.Fatalf("initTelemetry err = %v, want errTelemetryOff", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown: %v", err)
	}
	if _, on := profilingConfig(); on {
		t.Fatal("profiling on with everything unset")
	}
}

func TestTelemetrySwitches(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317"}, "http://alloy:4317"},
		{"blank endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "  "}, ""},
		{"legacy addr", map[string]string{"COLLECTOR_SERVICE_ADDR": "alloy:4317"}, "http://alloy:4317"},
		{"sdk disabled", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "OTEL_SDK_DISABLED": "true"}, ""},
		{"sdk disabled upper", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "OTEL_SDK_DISABLED": "TRUE"}, ""},
		{"sdk disabled false", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "OTEL_SDK_DISABLED": "false"}, "http://alloy:4317"},
		{"enable_tracing 0", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "ENABLE_TRACING": "0"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearTelemetryEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got := otlpEndpoint(); got != c.want {
				t.Errorf("otlpEndpoint() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestProfilingSwitch(t *testing.T) {
	cases := []struct {
		addr, enable string
		on           bool
	}{
		{"", "", false},
		{"", "1", false},
		{"http://alloy:4040", "", true},
		{"http://alloy:4040", "1", true},
		{"http://alloy:4040", "0", false},
	}
	for _, c := range cases {
		clearTelemetryEnv(t)
		t.Setenv("PYROSCOPE_ADDR", c.addr)
		t.Setenv("ENABLE_PROFILING", c.enable)
		if addr, on := profilingConfig(); on != c.on || (on && addr != c.addr) {
			t.Errorf("PYROSCOPE_ADDR=%q ENABLE_PROFILING=%q: on=%v addr=%q, want on=%v", c.addr, c.enable, on, addr, c.on)
		}
	}
}

func cfgEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func quietLog() *logrus.Logger {
	l := logrus.New()
	l.Out = io.Discard
	return l
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := configFromEnv(cfgEnv(nil), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BcryptCost != 11 || cfg.SessionTTL != 336*time.Hour {
		t.Errorf("cost %d, ttl %s", cfg.BcryptCost, cfg.SessionTTL)
	}
	if !cfg.LoginLimiter.Enabled() || !cfg.RegisterLimiter.Enabled() {
		t.Error("limiters must be on by default")
	}
	// 5 attempts, the 6th is refused
	for i := range 5 {
		if ok, _ := cfg.LoginLimiter.Allow("a"); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if ok, _ := cfg.LoginLimiter.Allow("a"); ok {
		t.Error("default login limit is not 5")
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := configFromEnv(cfgEnv(map[string]string{
		"BCRYPT_COST": " 4 ", "SESSION_TTL": "1h", "LOGIN_RATE_LIMIT": "0", "REGISTER_RATE_LIMIT": "2",
	}), quietLog())
	if err != nil || cfg.BcryptCost != 4 || cfg.SessionTTL != time.Hour {
		t.Fatalf("%+v, %v", cfg, err)
	}
	if cfg.LoginLimiter.Enabled() {
		t.Error("LOGIN_RATE_LIMIT=0 must switch the limiter off")
	}
	if !cfg.RegisterLimiter.Enabled() {
		t.Error("register limiter off")
	}
}

func TestConfigBadSecuritySettingsAreErrors(t *testing.T) {
	for _, m := range []map[string]string{
		{"BCRYPT_COST": "abc"}, {"BCRYPT_COST": "3"}, {"BCRYPT_COST": "17"}, {"BCRYPT_COST": "0"},
		{"SESSION_TTL": "forever"}, {"SESSION_TTL": "0s"}, {"SESSION_TTL": "-1h"},
	} {
		if _, err := configFromEnv(cfgEnv(m), quietLog()); err == nil {
			t.Errorf("%v accepted", m)
		}
	}
}

func TestConfigRateLimitTypoKeepsTheDefault(t *testing.T) {
	cfg, err := configFromEnv(cfgEnv(map[string]string{"LOGIN_RATE_LIMIT": "lots", "LOGIN_RATE_WINDOW": "soon"}), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LoginLimiter.Enabled() {
		t.Error("a typo switched the login limiter off")
	}
}
