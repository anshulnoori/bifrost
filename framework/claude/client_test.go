package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUsageIsAccountScopedAndSanitized(t *testing.T) {
	const token = "synthetic-bridge-token-for-tests-only"
	t.Setenv("CLAUDE_BRIDGE_TOKEN", token)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Claude-Bridge-Token") != token || r.Method != http.MethodGet {
			t.Fatal("unauthenticated usage request")
		}
		switch r.URL.Path {
		case "/accounts/account-a/usage":
			w.Write([]byte(`{
				"five_hour":{"utilization":6,"resets_at":"2026-10-02T20:00:00Z"},
				"seven_day":{"utilization":1.5,"resets_at":"not-a-time"},
				"seven_day_opus":null,
				"seven_day_sonnet":{"utilization":"bad"},
				"extra_usage":{"spend":"$12.34","organization_uuid":"private-org"},
				"checked_at":"2026-10-02T19:55:00.000Z",
				"limits":[
					{"kind":"weekly_scoped","percent":40,"resets_at":"2026-10-05T00:00:00Z","scope":{"model":{"display_name":"Fable"}}},
					{"kind":"spend","percent":99,"scope":{"model":{"display_name":"private-spend"}}}
				]}`))
		case "/accounts/account-b/usage":
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"detail":"private-native-error"}`))
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	usage, status, err := client.Usage(context.Background(), "account-a")
	if err != nil || status != 200 {
		t.Fatalf("usage failed: %d %v", status, err)
	}
	if *usage.FiveHour.Utilization != 6 || *usage.FiveHour.ResetsAt != "2026-10-02T20:00:00Z" {
		t.Fatalf("five-hour window: %+v", usage.FiveHour)
	}
	if usage.CheckedAt == nil || *usage.CheckedAt != "2026-10-02T19:55:00.000Z" {
		t.Fatal("bridge reading time not preserved")
	}
	if *usage.SevenDay.Utilization != 1.5 || usage.SevenDay.ResetsAt != nil {
		t.Fatalf("weekly window must keep percent and drop an invalid reset: %+v", usage.SevenDay)
	}
	if usage.SevenDayOpus != nil || usage.SevenDaySonnet != nil {
		t.Fatal("absent or malformed windows must not be fabricated")
	}
	if len(usage.Models) != 1 || usage.Models[0].Name != "Fable" || *usage.Models[0].Utilization != 40 {
		t.Fatalf("model windows: %+v", usage.Models)
	}
	wire, _ := json.Marshal(usage)
	if strings.Contains(string(wire), "private") || strings.Contains(string(wire), "spend") {
		t.Fatalf("private upstream fields exposed: %s", wire)
	}
	if _, status, _ := client.Usage(context.Background(), "account-b"); status != 409 {
		t.Fatalf("disconnected account status %d", status)
	}
	_, status, err = client.Usage(context.Background(), "account-c")
	if status != 502 || strings.Contains(err.Error(), "private") {
		t.Fatalf("native errors must be redacted: %d %v", status, err)
	}
	if _, status, _ := client.Usage(context.Background(), "../account-a"); status != 400 {
		t.Fatal("account path traversal accepted")
	}
}

func TestAboveReserveUsesOnlyTheWeeklyWindow(t *testing.T) {
	window := func(used float64) *UsageWindow { return &UsageWindow{Utilization: &used} }
	for _, tc := range []struct {
		name    string
		usage   *Usage
		reserve float64
		want    bool
	}{
		{"remaining above reserve", &Usage{SevenDay: window(70)}, 25, true},
		// At the boundary the reserve is reached: 25 remaining is not above 25.
		{"remaining equals reserve", &Usage{SevenDay: window(75)}, 25, false},
		{"remaining below reserve", &Usage{SevenDay: window(80)}, 25, false},
		// An exhausted five-hour window must not trip a weekly reserve.
		{"five-hour exhausted", &Usage{FiveHour: window(100), SevenDay: window(10)}, 25, true},
		{"weekly missing", &Usage{FiveHour: window(10)}, 25, false},
		{"weekly out of range", &Usage{SevenDay: window(101)}, 0, false},
		{"nil usage", nil, 25, false},
		{"invalid reserve", &Usage{SevenDay: window(10)}, 101, false},
	} {
		if got := tc.usage.AboveReserve(tc.reserve); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestExhaustedCountsEveryWindowThatGovernsTheModel(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	window := func(used float64, resets string) *UsageWindow {
		w := &UsageWindow{Utilization: &used}
		if resets != "" {
			w.ResetsAt = &resets
		}
		return w
	}
	later, earlier := "2026-10-02T15:00:00Z", "2026-10-02T11:00:00Z"
	for _, tc := range []struct {
		name  string
		usage *Usage
		model string
		want  bool
	}{
		{"room in every window", &Usage{FiveHour: window(99.5, later), SevenDay: window(60, later)}, "claude-sonnet-4-6", false},
		{"session used up", &Usage{FiveHour: window(100, later), SevenDay: window(10, later)}, "claude-sonnet-4-6", true},
		{"week used up", &Usage{FiveHour: window(0, later), SevenDay: window(100, later)}, "claude-haiku-4-5", true},
		// A cached reading can outlive the window it describes.
		{"session reset since the reading", &Usage{FiveHour: window(100, earlier), SevenDay: window(10, later)}, "claude-sonnet-4-6", false},
		{"no reset time", &Usage{FiveHour: window(100, "")}, "claude-sonnet-4-6", true},
		{"opus week used up, opus model", &Usage{SevenDayOpus: window(100, later)}, "claude-opus-4-5", true},
		{"opus week used up, sonnet model", &Usage{SevenDayOpus: window(100, later)}, "claude-sonnet-4-6", false},
		{"sonnet week used up, sonnet model", &Usage{SevenDaySonnet: window(100, later)}, "claude-sonnet-4-6", true},
		{"no windows", &Usage{}, "claude-sonnet-4-6", false},
		{"nil usage", nil, "claude-sonnet-4-6", false},
	} {
		if got := tc.usage.Exhausted(tc.model, now); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
