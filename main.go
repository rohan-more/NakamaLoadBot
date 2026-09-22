package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type config struct {
	url           string
	serverKey     string
	devicePrefix  string
	bots          int
	shootInterval time.Duration
	matchTimeout  time.Duration
	retryDelay    time.Duration
	duration      time.Duration
	rampDelay     time.Duration
	authAttempts  int
	authBackoff   time.Duration
	sampleEvery   time.Duration
	out           string
	label         string
	verbose       bool
	quiet         bool
}

func main() {
	var cfg config

	flag.StringVar(&cfg.url, "url", "http://127.0.0.1:7350", "Nakama server URL")
	flag.StringVar(&cfg.serverKey, "key", "defaultkey", "Nakama server key")
	flag.StringVar(&cfg.devicePrefix, "device-prefix", "loadbot", "prefix for generated device ids; reused across runs so accounts are stable")
	flag.IntVar(&cfg.bots, "bots", 2, "number of bots to run")
	// The server's shoot cooldown is 20 ticks at 10 ticks/sec, so anything
	// faster than 2s is discarded server side.
	flag.DurationVar(&cfg.shootInterval, "shoot-interval", 2500*time.Millisecond, "delay between shots")
	flag.DurationVar(&cfg.matchTimeout, "match-timeout", 90*time.Second, "give up on a match that never finishes")
	flag.DurationVar(&cfg.retryDelay, "retry-delay", 2*time.Second, "delay before retrying after a failed match")
	flag.DurationVar(&cfg.duration, "duration", 0, "how long to run; 0 runs until interrupted")
	flag.DurationVar(&cfg.rampDelay, "ramp-delay", 100*time.Millisecond, "delay between starting each bot")
	flag.IntVar(&cfg.authAttempts, "auth-attempts", 5, "attempts to authenticate before giving up on a bot")
	flag.DurationVar(&cfg.authBackoff, "auth-backoff", 250*time.Millisecond, "initial backoff between authentication attempts; doubles and is jittered")
	flag.DurationVar(&cfg.sampleEvery, "sample-every", time.Second, "how often to sample connected and in-match bots")
	flag.StringVar(&cfg.out, "out", "", "write a JSON report of the run to this path")
	flag.StringVar(&cfg.label, "label", "", "free-form label stored in the JSON report, to tell runs apart")
	flag.BoolVar(&cfg.verbose, "v", false, "log every state sync")
	flag.BoolVar(&cfg.quiet, "quiet", false, "suppress per-bot logging and print only the summary")
	flag.Parse()

	if cfg.bots < 1 {
		log.Fatal("-bots must be at least 1")
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if cfg.duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, cfg.duration)
		defer stop()
	}

	logger.Printf("starting %d bots against %s", cfg.bots, cfg.url)
	started := time.Now()

	var st stats
	rec := newRecorder()

	// Sampling outlives the bots' ctx only until they have all returned, so
	// the series covers the ramp down as well as the run.
	sampleCtx, stopSampling := context.WithCancel(context.Background())
	sampled := make(chan struct{})
	go rec.sampleCCU(sampleCtx, started, cfg.sampleEvery, sampled)

	var wg sync.WaitGroup

	for i := 1; i <= cfg.bots; i++ {
		// Stagger startup so a large run doesn't open every socket at once.
		if i > 1 && cfg.rampDelay > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(cfg.rampDelay):
			}
		}
		if ctx.Err() != nil {
			break
		}

		b := newBot(i, cfg, &st, rec, logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.run(ctx)
		}()
	}

	wg.Wait()
	stopSampling()
	<-sampled

	series, peakConnected, peakInMatch := rec.ccuSeries()
	report := runReport{
		Label:      cfg.label,
		StartedAt:  started,
		ElapsedSec: round(time.Since(started).Seconds(), 3),
		Config: runConfig{
			URL:             cfg.url,
			Bots:            cfg.bots,
			RampDelayMs:     cfg.rampDelay.Milliseconds(),
			ShootIntervalMs: cfg.shootInterval.Milliseconds(),
			DurationSec:     int64(cfg.duration.Seconds()),
		},
		Totals: runTotals{
			Matches:     st.matches.Load(),
			Shots:       st.shots.Load(),
			Wins:        st.wins.Load(),
			Timeouts:    st.timeouts.Load(),
			AuthRetries: st.authRetries.Load(),
			Errors:      st.errors.Load(),
		},
		PeakConnected: peakConnected,
		PeakInMatch:   peakInMatch,
		Ops:           rec.opReports(),
		CCU:           series,
	}

	logger.Printf("--- run complete in %s ---", time.Since(started).Truncate(time.Millisecond))
	logger.Printf("matches joined: %d", report.Totals.Matches)
	logger.Printf("shots sent:     %d", report.Totals.Shots)
	logger.Printf("wins:           %d", report.Totals.Wins)
	logger.Printf("timeouts:       %d", report.Totals.Timeouts)
	logger.Printf("auth retries:   %d", report.Totals.AuthRetries)
	logger.Printf("errors:         %d", report.Totals.Errors)
	report.printTable(os.Stdout)

	if cfg.out != "" {
		if err := report.writeJSON(cfg.out); err != nil {
			logger.Fatalf("write report: %v", err)
		}
		logger.Printf("report written to %s", cfg.out)
	}
}
