package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Reservations are conservative admission allowances, not invoices. Charge even
// failed/cancelled calls: a client timeout does not prove remote work stopped.
// $0.05 covers 180 seconds at the published T4/L4 + 2 CPU + 4 GiB base rates.
// Image builds, other callers, storage and regional premiums are not accounted.
const reservationUSD = 0.05
const alertReservations = 200 // proposed $10 calendar-month alert
const maxReservations = 500   // proposed $25 calendar-month cutoff

type costBudget struct {
	Month        string `json:"month"`
	Reservations int    `json:"reservations"`
}

var modalSlot = make(chan struct{}, 1) // one outbound request; zero waiting slots
var errCostGate = errors.New("headroom cost or concurrency gate closed")

// reserve persists before dispatch and fails closed for compression on corrupt
// state or I/O errors. The caller retains the original inference input.
func reserveCost(path string, now time.Time) (costBudget, error) {
	budget := costBudget{Month: now.UTC().Format("2006-01")}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return budget, errCostGate
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return budget, errCostGate
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	data, err := os.ReadFile(path)
	if err == nil {
		var previous costBudget
		if json.Unmarshal(data, &previous) != nil || previous.Reservations < 0 || previous.Reservations > maxReservations {
			return budget, errCostGate
		}
		month, err := time.Parse("2006-01", previous.Month)
		if err != nil || month.Format("2006-01") > budget.Month {
			return budget, errCostGate
		}
		if previous.Month == budget.Month {
			budget = previous
		}
	} else if !os.IsNotExist(err) {
		return budget, errCostGate
	}
	if budget.Reservations >= maxReservations {
		return budget, errCostGate
	}
	budget.Reservations++
	data, _ = json.Marshal(budget)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".headroom-budget-*")
	if err != nil {
		return budget, errCostGate
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || os.Rename(tmp.Name(), path) != nil {
		return budget, errCostGate
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return budget, errCostGate
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return budget, errCostGate
	}
	return budget, nil
}

func (b *bridge) admitModal(event *Event) (func(), error) {
	select {
	case modalSlot <- struct{}{}:
	default:
		return nil, errCostGate
	}
	release := func() { <-modalSlot }
	if b.config.CostLedgerPath != "" {
		budget, err := reserveCost(b.config.CostLedgerPath, time.Now())
		if event != nil {
			event.ReservedMonthUSD = float64(budget.Reservations) * reservationUSD
			event.BudgetAlert = budget.Reservations >= alertReservations
		}
		if err != nil {
			release()
			return nil, errCostGate
		}
		if event != nil {
			event.ReservedRequestUSD = reservationUSD
		}
	}
	return release, nil
}
