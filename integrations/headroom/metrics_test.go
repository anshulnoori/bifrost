package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestMonitorReload(t *testing.T) {
	t.Setenv("TEST_MONITOR_TOKEN", strings.Repeat("m", 32))
	config := Config{MetricsAddress: "127.0.0.1:0", MetricsTokenEnv: "TEST_MONITOR_TOKEN", RetentionSeconds: 60}
	if err := configureMonitor(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Cleanup() })
	config.RetentionSeconds = 0
	if err := configureMonitor(config); err != nil {
		t.Fatal("unchanged listener must permit policy reload", err)
	}
	t.Setenv("TEST_MONITOR_TOKEN", strings.Repeat("n", 32))
	if err := configureMonitor(config); err == nil {
		t.Fatal("credential rotation must require restart")
	}
}

func TestMetricsPrivacyRetentionAndRestart(t *testing.T) {
	l := &eventLedger{retention: time.Minute, totals: map[string]uint64{}}
	for i := 0; i < 1005; i++ {
		l.add(&Event{Started: time.Now(), Status: "bypassed", Model: "sensitive-model", Thread: "sensitive-thread", Quality: "not_evaluated"})
	}
	if len(l.events) != 1000 {
		t.Fatal("unbounded storage", len(l.events))
	}
	req := httptest.NewRequest("GET", "/v1/events", nil)
	resp := httptest.NewRecorder()
	l.handler("token").ServeHTTP(resp, req)
	if resp.Code != 401 {
		t.Fatal("unauthenticated metrics")
	}
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp = httptest.NewRecorder()
	l.handler("token").ServeHTTP(resp, req)
	if strings.Contains(resp.Body.String(), "sensitive") || !strings.Contains(resp.Body.String(), `status="bypassed"} 1005`) {
		t.Fatal("unsafe labels or wrong counts")
	}
	l.prune(time.Now().Add(2 * time.Minute))
	if len(l.events) != 0 {
		t.Fatal("expired metadata retained")
	}
	fresh := &eventLedger{totals: map[string]uint64{}}
	req = httptest.NewRequest("GET", "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp = httptest.NewRecorder()
	fresh.handler("token").ServeHTTP(resp, req)
	if gjson.GetBytes(resp.Body.Bytes(), "events.#").Int() != 0 || gjson.GetBytes(resp.Body.Bytes(), "quality").Str != "not_evaluated" || gjson.GetBytes(resp.Body.Bytes(), "cost_savings").Type != gjson.Null {
		t.Fatal("restart invented measurements")
	}
	fresh.add(&Event{Status: "compressed"})
	if len(fresh.events) != 0 || fresh.totals["compressed"] != 1 {
		t.Fatal("zero-retention not honored")
	}
}
