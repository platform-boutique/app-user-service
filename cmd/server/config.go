package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"user-service/internal/users"

	"github.com/sirupsen/logrus"
)

// configFromEnv reads the service settings:
//
//	BCRYPT_COST          bcrypt cost, 4-16 (default 11)
//	SESSION_TTL          Go duration, lifetime of a session (default 336h = 14 days)
//	LOGIN_RATE_LIMIT     login attempts per e-mail address and window, 0 = off (default 5)
//	LOGIN_RATE_WINDOW    Go duration (default 1m)
//	REGISTER_RATE_LIMIT  registrations per window for the whole process, 0 = off (default 60)
//	REGISTER_RATE_WINDOW Go duration (default 1m)
//
// A bad bcrypt cost or TTL is an error (the process exits), because both are security settings. A bad rate
// limit value falls back to its default and is logged, so a typo never switches the protection off.
func configFromEnv(getenv func(string) string, log *logrus.Logger) (users.Config, error) {
	cfg := users.Config{BcryptCost: users.DefaultBcryptCost, SessionTTL: users.DefaultSessionTTL}
	if v := strings.TrimSpace(getenv("BCRYPT_COST")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < users.MinBcryptCost || n > users.MaxBcryptCost {
			return cfg, fmt.Errorf("BCRYPT_COST=%q: want an integer from %d to %d", v, users.MinBcryptCost, users.MaxBcryptCost)
		}
		cfg.BcryptCost = n
	}
	if cfg.BcryptCost < 10 {
		log.Warnf("BCRYPT_COST=%d is below 10: fine for tests, too fast for real passwords", cfg.BcryptCost)
	}
	if v := strings.TrimSpace(getenv("SESSION_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("SESSION_TTL=%q: want a positive Go duration such as 336h", v)
		}
		cfg.SessionTTL = d
	}
	loginLimit, loginWindow, err := users.RateLimitFromEnv(getenv, "LOGIN", users.DefaultLoginLimit, users.DefaultLoginWindow)
	if err != nil {
		log.Warnf("login rate limit: %v", err)
	}
	if loginLimit > 0 {
		log.Infof("login rate limit: %d attempts per %s per e-mail address (per process)", loginLimit, loginWindow)
	} else {
		log.Warn("login rate limit is off (LOGIN_RATE_LIMIT=0)")
	}
	cfg.LoginLimiter = users.NewRateLimiter(loginLimit, loginWindow, nil)
	regLimit, regWindow, err := users.RateLimitFromEnv(getenv, "REGISTER", users.DefaultRegisterLimit, users.DefaultRegisterWindow)
	if err != nil {
		log.Warnf("register rate limit: %v", err)
	}
	if regLimit > 0 {
		log.Infof("register rate limit: %d per %s for the whole process", regLimit, regWindow)
	} else {
		log.Warn("register rate limit is off (REGISTER_RATE_LIMIT=0)")
	}
	cfg.RegisterLimiter = users.NewRateLimiter(regLimit, regWindow, nil)
	log.Infof("bcrypt cost %d, session ttl %s", cfg.BcryptCost, cfg.SessionTTL)
	return cfg, nil
}
