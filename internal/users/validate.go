package users

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	pb "user-service/proto"
)

// Limits of the API contract (see proto/users.proto).
const (
	MinEmailLen    = 3
	MaxEmailLen    = 254
	MinPasswordLen = 8
	MaxPasswordLen = 72 // bytes: bcrypt ignores everything after the 72nd
	MaxNameLen     = 100
)

// ErrInvalid marks a request that breaks the contract; the gRPC layer maps it to INVALID_ARGUMENT.
var ErrInvalid = errors.New("invalid argument")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// NormalizeEmail trims and lower-cases an address. It does not validate it.
func NormalizeEmail(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// ValidateEmail normalises (trim, lower-case) and checks a registration address:
// 3-254 characters, a plain address without display name or angle brackets.
func ValidateEmail(raw string) (string, error) {
	s := NormalizeEmail(raw)
	if !utf8.ValidString(s) || hasControl(s) {
		return "", invalid("email contains invalid characters")
	}
	if n := utf8.RuneCountInString(s); n < MinEmailLen || n > MaxEmailLen {
		return "", invalid("email must be %d-%d characters", MinEmailLen, MaxEmailLen)
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return "", invalid("email must be a plain address like name@example.com")
	}
	return s, nil
}

// ValidatePassword checks the length in BYTES (8-72). The password is not trimmed or changed in any way.
func ValidatePassword(p string) error {
	if n := len(p); n < MinPasswordLen || n > MaxPasswordLen {
		return invalid("password must be %d-%d bytes (got %d)", MinPasswordLen, MaxPasswordLen, n)
	}
	return nil
}

// ValidateName trims the display name and checks 1-100 characters without control characters.
func ValidateName(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !utf8.ValidString(s) || hasControl(s) {
		return "", invalid("name contains invalid characters")
	}
	if n := utf8.RuneCountInString(s); n < 1 || n > MaxNameLen {
		return "", invalid("name must be 1-%d characters", MaxNameLen)
	}
	return s, nil
}

// ValidateRegister checks and normalises a Register request.
func ValidateRegister(req *pb.RegisterRequest) (email, password, name string, err error) {
	if email, err = ValidateEmail(req.GetEmail()); err != nil {
		return
	}
	if err = ValidatePassword(req.GetPassword()); err != nil {
		return
	}
	if name, err = ValidateName(req.GetName()); err != nil {
		return
	}
	return email, req.GetPassword(), name, nil
}

// ValidateLogin only insists that both fields are present. Anything else that is wrong (an address that
// cannot exist, a password that is too long) is simply a failed login, so the answer never tells which
// part was wrong.
func ValidateLogin(req *pb.LoginRequest) (email, password string, err error) {
	email = NormalizeEmail(req.GetEmail())
	if email == "" {
		return "", "", invalid("email is required")
	}
	if req.GetPassword() == "" {
		return "", "", invalid("password is required")
	}
	return email, req.GetPassword(), nil
}
