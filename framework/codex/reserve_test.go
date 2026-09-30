package codex

import (
	"encoding/base64"
	"math"
	"testing"
)

func TestAboveReserve(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		used, secondary, reserve float64
		want                     bool
	}{
		{"above", 100, 74.9, 25, true},
		{"equal", 10, 75, 25, false},
		{"below", 10, 75.1, 25, false},
		{"weekly", 10, 76, 25, false},
		{"recovered", 10, 74, 25, true},
		{"five hour exhausted", 100, 10, 25, true},
		{"zero reserve exhausted", 0, 100, 0, false},
		{"full reserve", 0, 0, 100, false},
		{"invalid weekly usage", 0, math.NaN(), 25, false},
		{"invalid short usage ignored", math.NaN(), 0, 25, true},
		{"invalid reserve", 10, 0, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &Usage{Limits: &UsageLimits{Allowed: true, Primary: &UsageWindow{UsedPercent: &tc.used, WindowSeconds: 18000}, Secondary: &UsageWindow{UsedPercent: &tc.secondary, WindowSeconds: 604800}}}
			if got := u.AboveReserve(tc.reserve); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, u := range []*Usage{nil, {}, {Limits: &UsageLimits{Allowed: true}}, {Limits: &UsageLimits{Allowed: true, Primary: &UsageWindow{}}}} {
		if u.AboveReserve(25) {
			t.Fatal("unknown usage allowed")
		}
	}
	used := 1.0
	u := &Usage{Limits: &UsageLimits{Allowed: false, LimitReached: true, Primary: &UsageWindow{UsedPercent: &used, WindowSeconds: 604800}}}
	if !u.AboveReserve(25) {
		t.Fatal("weekly allowance in primary slot blocked by short-window flags")
	}
	u.Limits.Primary.WindowSeconds = 18000
	if u.AboveReserve(25) {
		t.Fatal("missing weekly window accepted")
	}
}

func TestTokenEmail(t *testing.T) {
	for _, tc := range []struct{ claims, want string }{
		{`{"email":"person@example.test"}`, "person@example.test"},
		{`{"https://api.openai.com/profile":{"email":"work@example.test"}}`, "work@example.test"},
		{`{"email":"first@example.test","https://api.openai.com/profile":{"email":"second@example.test"}}`, "first@example.test"},
		{`{"email":"bad\n@example.test"}`, ""}, {`{}`, ""}, {`not json`, ""},
	} {
		token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.claims)) + ".signature"
		if got := tokenEmail(token); got != tc.want {
			t.Fatalf("email=%q, want %q", got, tc.want)
		}
	}
}
