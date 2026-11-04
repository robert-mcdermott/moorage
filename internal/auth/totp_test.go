package auth

import (
	"encoding/hex"
	"testing"
	"time"
)

// TestTOTPVectors checks codes against RFC 6238's own, of which the last six digits are the app's.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for at, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130",
	} {
		if got := totpAt(secret, at/totpPeriod); got != want {
			t.Errorf("at %d: %s, want %s", at, got, want)
		}
	}
}

func TestCheckTOTP(t *testing.T) {
	secret, err := hex.DecodeString("3132333435363738393031323334353637383930")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1111111109, 0)
	current := now.Unix() / totpPeriod
	code := totpAt(secret, current)
	if step, ok := checkTOTP(secret, code, now); !ok || step != current {
		t.Errorf("%d %v", step, ok)
	}
	if step, ok := checkTOTP(secret, " "+code[:3]+" "+code[3:], now); !ok || step != current {
		t.Errorf("with spaces: %d %v", step, ok)
	}
	if step, ok := checkTOTP(secret, totpAt(secret, current-1), now); !ok || step != current-1 {
		t.Errorf("a step ago: %d %v", step, ok)
	}
	if step, ok := checkTOTP(secret, totpAt(secret, current+1), now); !ok || step != current+1 {
		t.Errorf("a step on: %d %v", step, ok)
	}
	for _, wrong := range []string{totpAt(secret, current-2), totpAt(secret, current+2), "", "12345", "1234567", "abcdef"} {
		if _, ok := checkTOTP(secret, wrong, now); ok {
			t.Errorf("%q was accepted", wrong)
		}
	}
}

func TestRecoveryCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		code := newRecoveryCode()
		if len(code) != 19 || seen[code] {
			t.Fatalf("%q", code)
		}
		seen[code] = true
	}
	if normalizeRecovery("ABCD-efgh 1234") != "abcdefgh1234" {
		t.Error(normalizeRecovery("ABCD-efgh 1234"))
	}
}
