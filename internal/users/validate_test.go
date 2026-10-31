package users

import (
	"errors"
	"strings"
	"testing"

	pb "user-service/proto"
)

func TestValidateEmail(t *testing.T) {
	good := map[string]string{
		"ada@example.com":       "ada@example.com",
		"  Ada@Example.COM \n":  "ada@example.com",
		"a+tag@sub.example.org": "a+tag@sub.example.org",
		"a@b":                   "a@b",
		"üser@example.com":      "üser@example.com",
		"ÜSER@EXAMPLE.COM":      "üser@example.com",
		strings.Repeat("a", 64) + "@" + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61): "", // 254 chars, see below
	}
	for in, want := range good {
		got, err := ValidateEmail(in)
		if want == "" { // the long one: only the length matters
			if len(in) > MaxEmailLen {
				t.Fatalf("test address is %d chars", len(in))
			}
			if err != nil {
				t.Errorf("254-char address rejected: %v", err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ValidateEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "   ", "ab", "plain", "@example.com", "ada@", "Ada <ada@example.com>", "<ada@example.com>",
		"ada@example.com, bob@example.com", "ada @example.com", "ada@exa\x00mple.com", "ada@example.com\x00",
		"a\nb@example.com", "\xff\xfe@example.com",
		strings.Repeat("a", 250) + "@b.cd", // 255 characters
	}
	for _, in := range bad {
		if got, err := ValidateEmail(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateEmail(%q) = %q, %v; want ErrInvalid", in, got, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	for _, p := range []string{strings.Repeat("a", 8), strings.Repeat("a", 72), "  spaces  kept  ", "pässwörd", strings.Repeat("é", 36)} {
		if err := ValidatePassword(p); err != nil {
			t.Errorf("%d bytes rejected: %v", len(p), err)
		}
	}
	for _, p := range []string{"", "short", strings.Repeat("a", 7), strings.Repeat("a", 73), strings.Repeat("é", 37)} { // é is 2 bytes
		err := ValidatePassword(p)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%d bytes accepted", len(p))
			continue
		}
		if strings.Contains(err.Error(), p) && p != "" {
			t.Errorf("error repeats the password: %v", err)
		}
	}
}

func TestValidateName(t *testing.T) {
	got, err := ValidateName("  Ada Lovelace \t")
	if err != nil || got != "Ada Lovelace" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := ValidateName(strings.Repeat("é", 100)); err != nil {
		t.Errorf("100 characters rejected: %v", err)
	}
	for _, in := range []string{"", "   ", strings.Repeat("é", 101), "Ada\x00", "Ada\nL", "\xff"} {
		if _, err := ValidateName(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateName(%q) accepted", in)
		}
	}
}

func TestValidateRegisterNamesTheField(t *testing.T) {
	ok := &pb.RegisterRequest{Email: "ada@example.com", Password: "correct horse", Name: "Ada"}
	email, pw, name, err := ValidateRegister(ok)
	if err != nil || email != "ada@example.com" || pw != "correct horse" || name != "Ada" {
		t.Fatalf("valid request: %q %q %q %v", email, pw, name, err)
	}
	for field, req := range map[string]*pb.RegisterRequest{
		"email":    {Email: "nope", Password: "correct horse", Name: "Ada"},
		"password": {Email: "ada@example.com", Password: "short", Name: "Ada"},
		"name":     {Email: "ada@example.com", Password: "correct horse", Name: " "},
	} {
		_, _, _, err := ValidateRegister(req)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v", field, err)
		}
	}
	// the password is not trimmed
	_, pw, _, err = ValidateRegister(&pb.RegisterRequest{Email: "ada@example.com", Password: "  pad  pad  ", Name: "Ada"})
	if err != nil || pw != "  pad  pad  " {
		t.Errorf("password changed: %q, %v", pw, err)
	}
}

func TestValidateLogin(t *testing.T) {
	email, pw, err := ValidateLogin(&pb.LoginRequest{Email: " ADA@Example.com ", Password: "x"})
	if err != nil || email != "ada@example.com" || pw != "x" {
		t.Errorf("got %q %q %v", email, pw, err)
	}
	// a malformed address or a short password is a failed login, not a validation error
	if _, _, err := ValidateLogin(&pb.LoginRequest{Email: "not an address", Password: "x"}); err != nil {
		t.Errorf("malformed address: %v", err)
	}
	for _, r := range []*pb.LoginRequest{{Password: "x"}, {Email: "  ", Password: "x"}, {Email: "ada@example.com"}} {
		if _, _, err := ValidateLogin(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v accepted", r)
		}
	}
}
