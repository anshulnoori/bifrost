package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
