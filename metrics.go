package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Operations timed per bot. shot_to_hit is the round trip from sending a
// shot to seeing the opponent's health drop in a state sync, so it includes
// up to one server tick (100ms at 10 ticks/sec) of quantisation.
const (
	opAuth      = "auth"
	opAccount   = "account"
	opConnect   = "connect"
	opFindMatch = "find_match"
	opJoin      = "join"
	opShoot     = "shoot_send"
	opShotToHit = "shot_to_hit"
)

var opOrder = []string{opAuth, opAccount, opConnect, opFindMatch, opJoin, opShoot, opShotToHit}

type opStats struct {
	mu       sync.Mutex
	samples  []time.Duration
	attempts int64
	failures int64
}

type ccuSample struct {
	T         float64 `json:"t"`
	Connected int64   `json:"connected"`
	InMatch   int64   `json:"in_match"`
}

// recorder is shared by every bot.
type recorder struct {
	mu  sync.Mutex
	ops map[string]*opStats

	// Sockets currently open, and bots currently inside a match.
	connected atomic.Int64
	inMatch   atomic.Int64

	seriesMu sync.Mutex
	series   []ccuSample
}

func newRecorder() *recorder {
	return &recorder{ops: make(map[string]*opStats)}
}

func (r *recorder) op(name string) *opStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.ops[name]
	if !ok {
		o = &opStats{}
		r.ops[name] = o
	}
	return o
}

// observe records one attempt at an operation. Only successes feed the
// latency percentiles, since a failure's duration says more about timeouts
// than about the server. Errors caused by the run itself shutting down are
// not counted at all.
func (r *recorder) observe(ctx context.Context, name string, start time.Time, err error) {
	if err != nil && ctx.Err() != nil {
		return
	}
	d := time.Since(start)
	o := r.op(name)
	o.mu.Lock()
	o.attempts++
	if err != nil {
		o.failures++
	} else {
		o.samples = append(o.samples, d)
	}
	o.mu.Unlock()
}

// sample records a latency that has no failure mode of its own.
func (r *recorder) sample(name string, d time.Duration) {
	o := r.op(name)
	o.mu.Lock()
	o.attempts++
	o.samples = append(o.samples, d)
	o.mu.Unlock()
}

// sampleCCU snapshots the gauges until ctx is done.
func (r *recorder) sampleCCU(ctx context.Context, start time.Time, every time.Duration, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.seriesMu.Lock()
		r.series = append(r.series, ccuSample{
			T:         math.Round(time.Since(start).Seconds()*10) / 10,
			Connected: r.connected.Load(),
			InMatch:   r.inMatch.Load(),
		})
		r.seriesMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type opReport struct {
	Op          string  `json:"op"`
	Attempts    int64   `json:"attempts"`
	Failures    int64   `json:"failures"`
	FailureRate float64 `json:"failure_rate"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	P99Ms       float64 `json:"p99_ms"`
	MaxMs       float64 `json:"max_ms"`
}

func (r *recorder) opReports() []opReport {
	var out []opReport
	for _, name := range opOrder {
		r.mu.Lock()
		o, ok := r.ops[name]
		r.mu.Unlock()
		if !ok {
			continue
		}
		o.mu.Lock()
		samples := append([]time.Duration(nil), o.samples...)
		rep := opReport{Op: name, Attempts: o.attempts, Failures: o.failures}
		o.mu.Unlock()

		if rep.Attempts > 0 {
			rep.FailureRate = round(float64(rep.Failures)/float64(rep.Attempts), 4)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		rep.P50Ms = percentileMs(samples, 0.50)
		rep.P95Ms = percentileMs(samples, 0.95)
		rep.P99Ms = percentileMs(samples, 0.99)
		rep.MaxMs = percentileMs(samples, 1.00)
		out = append(out, rep)
	}
	return out
}

func (r *recorder) ccuSeries() (series []ccuSample, peakConnected, peakInMatch int64) {
	r.seriesMu.Lock()
	defer r.seriesMu.Unlock()
	series = append([]ccuSample(nil), r.series...)
	for _, s := range series {
		peakConnected = max(peakConnected, s.Connected)
		peakInMatch = max(peakInMatch, s.InMatch)
	}
	return series, peakConnected, peakInMatch
}

// percentileMs uses nearest-rank on an already sorted slice.
func percentileMs(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	idx = min(max(idx, 0), len(sorted)-1)
	return round(float64(sorted[idx])/float64(time.Millisecond), 2)
}

func round(v float64, places int) float64 {
	pow := math.Pow(10, float64(places))
	return math.Round(v*pow) / pow
}

type runTotals struct {
	Matches     int64 `json:"matches"`
	Created     int64 `json:"created"`
	Shots       int64 `json:"shots"`
	Wins        int64 `json:"wins"`
	Timeouts    int64 `json:"timeouts"`
	AuthRetries int64 `json:"auth_retries"`
	Errors      int64 `json:"errors"`
}

type runConfig struct {
	URL             string `json:"url"`
	Bots            int    `json:"bots"`
	RampDelayMs     int64  `json:"ramp_delay_ms"`
	ShootIntervalMs int64  `json:"shoot_interval_ms"`
	DurationSec     int64  `json:"duration_sec"`
}

type runReport struct {
	Label         string      `json:"label,omitempty"`
	StartedAt     time.Time   `json:"started_at"`
	ElapsedSec    float64     `json:"elapsed_sec"`
	Config        runConfig   `json:"config"`
	Totals        runTotals   `json:"totals"`
	PeakConnected int64       `json:"peak_connected"`
	PeakInMatch   int64       `json:"peak_in_match"`
	Ops           []opReport  `json:"ops"`
	CCU           []ccuSample `json:"ccu"`
}

func (rep runReport) printTable(w io.Writer) {
	fmt.Fprintf(w, "peak connected: %d   peak in match: %d\n", rep.PeakConnected, rep.PeakInMatch)
	fmt.Fprintf(w, "%-12s %9s %8s %9s %9s %9s %9s\n", "op", "attempts", "fail%", "p50 ms", "p95 ms", "p99 ms", "max ms")
	for _, o := range rep.Ops {
		fmt.Fprintf(w, "%-12s %9d %7.2f%% %9.2f %9.2f %9.2f %9.2f\n",
			o.Op, o.Attempts, o.FailureRate*100, o.P50Ms, o.P95Ms, o.P99Ms, o.MaxMs)
	}
}

func (rep runReport) writeJSON(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
