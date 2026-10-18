package user

import "testing"

func TestEWalletNumber(t *testing.T) {
	cases := map[string]bool{
		"081234567890":      true,
		"+6281234567890":    true,
		"6281234567890":     true,
		"0812 3456 7890":    true, // dinormalisasi dulu
		"0212345678":        false,
		"0812":              false,
		"08123456789012345": false,
	}
	for in, want := range cases {
		if got := ewalletNumberRe.MatchString(normalizePhone(in)); got != want {
			t.Errorf("%q → %v, harap %v", in, got, want)
		}
	}
	if got := MaskAccount("081234567890"); got != "0812••••7890" {
		t.Errorf("mask = %q", got)
	}
}
