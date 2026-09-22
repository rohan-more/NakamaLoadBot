// Command ladder runs the load bot across a grid of matchmaking modes, start
// ramps and bot counts, recreating the server between every run so runs don't
// inherit each other's matches, and collects the results into one table.
//
// Each run's JSON report and log are kept alongside summary.csv and
// summary.md in the output directory.
package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type options struct {
	serverDir     string
	loadbot       string
	url           string
	outDir        string
	modes         []string
	bots          []int
	ramps         []time.Duration
	duration      time.Duration
	shootInterval time.Duration
	healthTimeout time.Duration
}

func main() {
	var (
		opts               options
		modes, bots, ramps string
	)
	defaultBin := "./loadbot"
	if runtime.GOOS == "windows" {
		defaultBin = "./loadbot.exe"
	}

	flag.StringVar(&opts.serverDir, "server-dir", "../NakamaLoadServer", "NakamaLoadServer checkout containing docker-compose.yml")
	flag.StringVar(&opts.loadbot, "loadbot", defaultBin, "path to a built load bot binary")
	flag.StringVar(&opts.url, "url", "http://127.0.0.1:7350", "Nakama server URL")
	flag.StringVar(&opts.outDir, "out-dir", "", "where to write results; defaults to results/ladder-<timestamp>")
	flag.StringVar(&modes, "modes", "naive,serialized,seats", "comma separated MATCHMAKING_MODE values to run")
	flag.StringVar(&bots, "bots", "20,100,500", "comma separated bot counts")
	flag.StringVar(&ramps, "ramps", "100ms,0s", "comma separated delays between starting each bot")
	flag.DurationVar(&opts.duration, "duration", 60*time.Second, "how long each run lasts")
	flag.DurationVar(&opts.shootInterval, "shoot-interval", 2500*time.Millisecond, "passed through to the bot")
	flag.DurationVar(&opts.healthTimeout, "health-timeout", 2*time.Minute, "how long to wait for the server after recreating it")
	flag.Parse()

	var err error
	if opts.modes = splitList(modes); len(opts.modes) == 0 {
		log.Fatal("-modes is empty")
	}
	if opts.bots, err = parseInts(bots); err != nil {
		log.Fatalf("-bots: %v", err)
	}
	if opts.ramps, err = parseDurations(ramps); err != nil {
		log.Fatalf("-ramps: %v", err)
	}
	if opts.loadbot, err = filepath.Abs(opts.loadbot); err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(opts.loadbot); err != nil {
		log.Fatalf("load bot binary: %v (build it first)", err)
	}
	if opts.outDir == "" {
		opts.outDir = filepath.Join("results", "ladder-"+time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	if err := compose(opts, "", "up", "-d", "postgres"); err != nil {
		log.Fatalf("start postgres: %v", err)
	}

	total := len(opts.modes) * len(opts.ramps) * len(opts.bots)
	log.Printf("%d runs of %s each, writing to %s", total, opts.duration, opts.outDir)

	var rows []row
	n := 0
	for _, mode := range opts.modes {
		for _, ramp := range opts.ramps {
			for _, count := range opts.bots {
				n++
				log.Printf("[%d/%d] mode=%s ramp=%s bots=%d", n, total, mode, ramp, count)
				r, err := runOne(opts, mode, ramp, count)
				if err != nil {
					log.Fatalf("mode=%s ramp=%s bots=%d: %v", mode, ramp, count, err)
				}
				log.Printf("        matches=%d created=%d join_fail=%.1f%% orphans=%d errors=%d",
					r.Matches, r.Created, r.JoinFailRate*100, r.Orphans, r.Errors)
				rows = append(rows, r)
			}
		}
	}

	// Leave the server as it normally runs.
	if err := recreate(opts, "seats"); err != nil {
		log.Printf("restore default mode: %v", err)
	}

	if err := writeCSV(filepath.Join(opts.outDir, "summary.csv"), rows); err != nil {
		log.Fatal(err)
	}
	md := markdown(rows)
	if err := os.WriteFile(filepath.Join(opts.outDir, "summary.md"), []byte(md), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Print(md)
}

// row is one run, flattened for the summary.
type row struct {
	Mode           string
	RampMs         int64
	Bots           int
	ElapsedSec     float64
	Matches        int64
	Created        int64
	MatchesPerMin  float64
	Timeouts       int64
	Errors         int64
	AuthRetries    int64
	JoinAttempts   int64
	JoinFailures   int64
	JoinFailRate   float64
	FindMatchP95   float64
	JoinP95        float64
	ShotToHitP50   float64
	PeakConnected  int64
	PeakInMatch    int64
	ServerCreated  int
	ServerStarted  int
	ServerFinished int
	ServerIdle     int
	// Matches created server side that never got a second player.
	Orphans int
}

func runOne(opts options, mode string, ramp time.Duration, count int) (row, error) {
	if err := recreate(opts, mode); err != nil {
		return row{}, err
	}

	name := fmt.Sprintf("%s-%dms-%d", mode, ramp.Milliseconds(), count)
	reportPath := filepath.Join(opts.outDir, name+".json")
	logFile, err := os.Create(filepath.Join(opts.outDir, name+".log"))
	if err != nil {
		return row{}, err
	}
	defer logFile.Close()

	cmd := exec.Command(opts.loadbot,
		"-url", opts.url,
		"-bots", strconv.Itoa(count),
		"-ramp-delay", ramp.String(),
		"-duration", opts.duration.String(),
		"-shoot-interval", opts.shootInterval.String(),
		"-quiet",
		"-label", name,
		"-out", reportPath,
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Run(); err != nil {
		return row{}, fmt.Errorf("load bot: %w", err)
	}

	rep, err := readReport(reportPath)
	if err != nil {
		return row{}, err
	}

	// The container was recreated for this run, so its log covers exactly it.
	serverLog, err := composeOutput(opts, "", "logs", "--no-color", "nakama")
	if err != nil {
		return row{}, fmt.Errorf("server logs: %w", err)
	}
	if err := os.WriteFile(filepath.Join(opts.outDir, name+".server.log"), serverLog, 0o644); err != nil {
		return row{}, err
	}

	r := row{
		Mode:          mode,
		RampMs:        ramp.Milliseconds(),
		Bots:          count,
		ElapsedSec:    rep.ElapsedSec,
		Matches:       rep.Totals.Matches,
		Created:       rep.Totals.Created,
		Timeouts:      rep.Totals.Timeouts,
		Errors:        rep.Totals.Errors,
		AuthRetries:   rep.Totals.AuthRetries,
		PeakConnected: rep.PeakConnected,
		PeakInMatch:   rep.PeakInMatch,

		ServerCreated:  bytes.Count(serverLog, []byte("Match initialised")),
		ServerStarted:  bytes.Count(serverLog, []byte("Match started with")),
		ServerFinished: bytes.Count(serverLog, []byte(`"msg":"Match over"`)),
		ServerIdle:     bytes.Count(serverLog, []byte("Closing idle match")),
	}
	r.Orphans = r.ServerCreated - r.ServerStarted
	if rep.ElapsedSec > 0 {
		r.MatchesPerMin = round(float64(r.Matches)/(rep.ElapsedSec/60), 1)
	}
	for _, o := range rep.Ops {
		switch o.Op {
		case "find_match":
			r.FindMatchP95 = o.P95Ms
		case "join":
			r.JoinAttempts, r.JoinFailures = o.Attempts, o.Failures
			r.JoinFailRate = o.FailureRate
			r.JoinP95 = o.P95Ms
		case "shot_to_hit":
			r.ShotToHitP50 = o.P50Ms
		}
	}
	return r, nil
}

// recreate restarts nakama in the given mode and waits until it is serving
// and has logged that mode, so a run can never silently measure the wrong one.
func recreate(opts options, mode string) error {
	if err := compose(opts, mode, "up", "-d", "--force-recreate", "--no-deps", "nakama"); err != nil {
		return fmt.Errorf("recreate nakama: %w", err)
	}

	deadline := time.Now().Add(opts.healthTimeout)
	want := []byte("Matchmaking mode: " + mode)
	for time.Now().Before(deadline) {
		if healthy(opts.url) {
			out, err := composeOutput(opts, "", "logs", "--no-color", "nakama")
			if err == nil && bytes.Contains(out, want) {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("nakama not healthy in mode %q within %s", mode, opts.healthTimeout)
}

func healthy(url string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(url, "/") + "/healthcheck")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func compose(opts options, mode string, args ...string) error {
	out, err := composeOutput(opts, mode, args...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func composeOutput(opts options, mode string, args ...string) ([]byte, error) {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = opts.serverDir
	cmd.Env = os.Environ()
	if mode != "" {
		cmd.Env = append(cmd.Env, "MATCHMAKING_MODE="+mode)
	}
	return cmd.CombinedOutput()
}

// report mirrors the parts of the load bot's JSON report the summary uses.
type report struct {
	ElapsedSec float64 `json:"elapsed_sec"`
	Totals     struct {
		Matches     int64 `json:"matches"`
		Created     int64 `json:"created"`
		Timeouts    int64 `json:"timeouts"`
		AuthRetries int64 `json:"auth_retries"`
		Errors      int64 `json:"errors"`
	} `json:"totals"`
	PeakConnected int64 `json:"peak_connected"`
	PeakInMatch   int64 `json:"peak_in_match"`
	Ops           []struct {
		Op          string  `json:"op"`
		Attempts    int64   `json:"attempts"`
		Failures    int64   `json:"failures"`
		FailureRate float64 `json:"failure_rate"`
		P50Ms       float64 `json:"p50_ms"`
		P95Ms       float64 `json:"p95_ms"`
	} `json:"ops"`
}

func readReport(path string) (report, error) {
	var rep report
	data, err := os.ReadFile(path)
	if err != nil {
		return rep, err
	}
	return rep, json.Unmarshal(data, &rep)
}

var csvHeader = []string{
	"mode", "ramp_ms", "bots", "elapsed_sec", "matches", "created", "matches_per_min",
	"timeouts", "errors", "auth_retries", "join_attempts", "join_failures", "join_fail_rate",
	"find_match_p95_ms", "join_p95_ms", "shot_to_hit_p50_ms", "peak_connected", "peak_in_match",
	"server_created", "server_started", "server_finished", "server_idle_closed", "orphans",
}

func writeCSV(path string, rows []row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{
			r.Mode, i64(r.RampMs), strconv.Itoa(r.Bots), f64(r.ElapsedSec), i64(r.Matches), i64(r.Created),
			f64(r.MatchesPerMin), i64(r.Timeouts), i64(r.Errors), i64(r.AuthRetries),
			i64(r.JoinAttempts), i64(r.JoinFailures), f64(r.JoinFailRate),
			f64(r.FindMatchP95), f64(r.JoinP95), f64(r.ShotToHitP50),
			i64(r.PeakConnected), i64(r.PeakInMatch),
			strconv.Itoa(r.ServerCreated), strconv.Itoa(r.ServerStarted),
			strconv.Itoa(r.ServerFinished), strconv.Itoa(r.ServerIdle), strconv.Itoa(r.Orphans),
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func markdown(rows []row) string {
	var b strings.Builder
	b.WriteString("| mode | ramp | bots | matches | created | matches/min | join fail | orphans | errors | find_match p95 | join p95 |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %dms | %d | %d | %d | %.1f | %.1f%% | %d | %d | %.2f ms | %.2f ms |\n",
			r.Mode, r.RampMs, r.Bots, r.Matches, r.Created, r.MatchesPerMin,
			r.JoinFailRate*100, r.Orphans, r.Errors, r.FindMatchP95, r.JoinP95)
	}
	return b.String()
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, part := range splitList(s) {
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("bad count %q", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no counts given")
	}
	return out, nil
}

func parseDurations(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, part := range splitList(s) {
		d, err := time.ParseDuration(part)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("bad duration %q", part)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ramps given")
	}
	return out, nil
}

func i64(v int64) string   { return strconv.FormatInt(v, 10) }
func f64(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func round(v float64, places int) float64 {
	pow := 1.0
	for i := 0; i < places; i++ {
		pow *= 10
	}
	return float64(int64(v*pow+0.5)) / pow
}
