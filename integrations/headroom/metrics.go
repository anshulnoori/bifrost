package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// Event contains metadata only. Token estimates refer to selected tool results,
// not the full prompt. Missing usage/cost/evaluation is unknown, never zero loss.
type Event struct {
	Done           atomic.Bool     `json:"-"`
	Started        time.Time       `json:"started"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	Project        string          `json:"project"`
	Principal      string          `json:"principal_hash"`
	Thread         string          `json:"thread_hash"`
	Status         string          `json:"status"`
	Reason         string          `json:"reason"`
	Eligible       bool            `json:"eligible"`
	Quality        string          `json:"quality"`
	Estimate       *estimate       `json:"tool_result_estimate"`
	CompressionMS  float64         `json:"compression_ms"`
	TaskMS         float64         `json:"plugin_attempt_ms"`
	Usage          json.RawMessage `json:"provider_usage"`
	ProviderFailed bool            `json:"provider_failed"`
	NetworkRetries int             `json:"network_retries"`
}

type eventLedger struct {
	mu        sync.Mutex
	events    []json.RawMessage
	times     []time.Time
	retention time.Duration
	totals    map[string]uint64
}

var ledger = eventLedger{retention: 15 * time.Minute, totals: map[string]uint64{}}
var monitorMu sync.Mutex
var monitor *http.Server
var monitorToken string

func (l *eventLedger) prune(now time.Time) {
	n := 0
	for n < len(l.times) && (now.Sub(l.times[n]) > l.retention || len(l.times)-n > 1000) {
		n++
	}
	if n > 0 {
		l.events = append([]json.RawMessage(nil), l.events[n:]...)
		l.times = append([]time.Time(nil), l.times[n:]...)
	}
}
func (l *eventLedger) add(event *Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.totals[event.Status]++
	if l.retention <= 0 {
		return
	}
	l.events = append(l.events, data)
	l.times = append(l.times, time.Now())
	l.prune(time.Now())
}
func (l *eventLedger) handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.prune(time.Now())
		switch r.URL.Path {
		case "/v1/events":
			w.Header().Set("Content-Type", "application/json")
			events := l.events
			if events == nil {
				events = []json.RawMessage{}
			}
			json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "events": events, "retention_seconds": int(l.retention.Seconds()), "limit": 1000, "quality": "not_evaluated", "ccr": "unsupported", "cost_savings": nil, "attempt_coverage": "primary_and_fallback_hooks; internal_retry_usage_unknown"})
		case "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			fmt.Fprintln(w, "# HELP bifrost_headroom_attempts_total Completed plugin attempts, not network requests\n# TYPE bifrost_headroom_attempts_total counter")
			for _, status := range []string{"compressed", "bypassed", "failed"} {
				fmt.Fprintf(w, "bifrost_headroom_attempts_total{status=%q} %d\n", status, l.totals[status])
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func configureMonitor(config Config) error {
	if config.RetentionSeconds < 0 || config.RetentionSeconds > 86400 {
		return errors.New("retention_seconds must be 0..86400")
	}
	monitorMu.Lock()
	defer monitorMu.Unlock()
	// Changing listener settings requires a gateway restart. Init may run during
	// reload while the old plugin still serves requests; never close its listener
	// or replace its credentials before the new configuration has validated.
	token := os.Getenv(config.MetricsTokenEnv)
	if monitor != nil && (monitor.Addr != config.MetricsAddress || monitorToken != token) {
		return errors.New("headroom monitor configuration requires gateway restart")
	}
	if monitor == nil && config.MetricsAddress != "" {
		host, _, err := net.SplitHostPort(config.MetricsAddress)
		if err != nil || (host != "127.0.0.1" && host != "::1") {
			return errors.New("metrics_address must bind a numeric loopback address")
		}
		if len(token) < 32 {
			return errors.New("metrics_token_env must resolve to at least 32 bytes")
		}
		listener, err := net.Listen("tcp", config.MetricsAddress)
		if err != nil {
			return err
		}
		monitorToken = token
		monitor = &http.Server{Addr: config.MetricsAddress, Handler: ledger.handler(token), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
		server := monitor
		go server.Serve(listener)
	}
	ledger.mu.Lock()
	ledger.retention = time.Duration(config.RetentionSeconds) * time.Second
	ledger.prune(time.Now())
	ledger.mu.Unlock()
	return nil
}

func providerUsage(resp *schemas.BifrostResponse) json.RawMessage {
	if resp == nil {
		return nil
	}
	var usage any
	switch {
	case resp.ChatResponse != nil:
		if resp.ChatResponse.Usage == nil {
			return nil
		}
		usage = resp.ChatResponse.Usage
	case resp.ResponsesResponse != nil:
		if resp.ResponsesResponse.Usage == nil {
			return nil
		}
		usage = resp.ResponsesResponse.Usage
	case resp.PassthroughResponse != nil:
		// Prefer provider-native fields, including unknown extensions.
		u := gjson.GetBytes(resp.PassthroughResponse.Body, "usage")
		if u.IsObject() {
			return json.RawMessage(u.Raw)
		}
		if resp.PassthroughResponse.PassthroughUsage == nil || resp.PassthroughResponse.PassthroughUsage.LLMUsage == nil {
			return nil
		}
		usage = resp.PassthroughResponse.PassthroughUsage.LLMUsage
	default:
		return nil
	}
	data, err := schemas.MarshalSorted(usage)
	if err != nil {
		return nil
	}
	return data
}
