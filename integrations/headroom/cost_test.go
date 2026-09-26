package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCostReservationsPersistAndStopBeforeDispatch(t *testing.T) {
	calls := 0
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { calls++; goodReply(w, r) })
	b.config.CostLedgerPath = filepath.Join(t.TempDir(), "budget.json")
	month := time.Now().UTC().Format("2006-01")
	write := func(n int) {
		t.Helper()
		data, _ := json.Marshal(costBudget{Month: month, Reservations: n})
		if err := os.WriteFile(b.config.CostLedgerPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(199)
	ctx := admitted("project-a", "vk-a")
	b.pre(ctx, chatRequest())
	event := ctx.Value(eventKey).(*Event)
	if !event.BudgetAlert || event.ReservedMonthUSD != 10 || event.ReservedRequestUSD != 0.05 || calls != 1 {
		t.Fatalf("alert boundary: %+v calls=%d", event, calls)
	}
	write(499)
	ctx = admitted("project-a", "vk-a")
	b.pre(ctx, chatRequest())
	if ctx.Value(eventKey).(*Event).ReservedMonthUSD != 25 || calls != 2 {
		t.Fatal("last permitted call failed")
	}
	// Recreate the bridge to prove the circuit survives plugin/process reload.
	b.config.FailurePolicy = "closed"
	reloaded, err := newBridge(b.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.client.CloseIdleConnections()
	ctx = admitted("project-a", "vk-a")
	req := chatRequest()
	out, sc, err := reloaded.pre(ctx, req)
	if out != req || sc != nil || err != nil || calls != 2 || ctx.Value(eventKey).(*Event).Reason != "budget_or_concurrency" {
		t.Fatal("budget must bypass without dispatch or blocking inference")
	}
}

func TestCostReservationCorruptionRolloverAndQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	now := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	first, err := reserveCost(path, now)
	if err != nil || first.Reservations != 1 {
		t.Fatal(first, err)
	}
	second, err := reserveCost(path, now.Add(time.Second))
	if err != nil || second.Month != "2026-10" || second.Reservations != 1 {
		t.Fatal(second, err)
	}
	if _, err := reserveCost(path, now); err == nil {
		t.Fatal("clock rollback must not reset budget")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveCost(path, now); err == nil {
		t.Fatal("corruption must block calls")
	}
	b := &bridge{}
	release, err := b.admitModal(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if queued, err := b.admitModal(nil); err == nil {
		queued()
		t.Fatal("excess work must be rejected without a queue")
	}
}

func TestFailedCallsRemainReserved(t *testing.T) {
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	b.config.CostLedgerPath = filepath.Join(t.TempDir(), "budget.json")
	ctx := admitted("project-a", "vk-a")
	req := chatRequest()
	out, sc, err := b.pre(ctx, req)
	if out != req || sc != nil || err != nil {
		t.Fatal("unavailable Modal must retain original inference")
	}
	data, err := os.ReadFile(b.config.CostLedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var budget costBudget
	if json.Unmarshal(data, &budget) != nil || budget.Reservations != 1 {
		t.Fatal("failed remote work must remain charged")
	}
}
