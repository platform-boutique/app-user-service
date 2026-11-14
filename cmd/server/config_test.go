package main

import (
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

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
