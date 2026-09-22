package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync/atomic"
	"time"

	nakama "github.com/ascii8/nakama-go"
)

// stats is shared across bots, so every field is touched atomically.
type stats struct {
	matches atomic.Int64
	// Matches this bot created rather than found. Ideal pairing creates one
	// match per two joins, so anything above half of matches is over-creation.
	created  atomic.Int64
	shots    atomic.Int64
	wins     atomic.Int64
	timeouts atomic.Int64
	errors   atomic.Int64
	// Connect bursts get refused at the TCP layer before Nakama ever sees
	// them, so retries are tracked separately from outright failures.
	authRetries atomic.Int64
}

type bot struct {
	id       int
	deviceID string
	cfg      config
	stats    *stats
	rec      *recorder
	log      *log.Logger

	// Resolved after authenticating; used to tell our own wins apart.
	userID   string
	username string

	// Match data arrives on the connection's reader goroutine; the play loop
	// consumes it from here so handlers never block the socket.
	data chan *nakama.MatchDataMsg
}

// matchTrack is per-match state used to attribute shot round trips.
type matchTrack struct {
	started   bool
	oppHealth int       // -1 until the first state sync
	shotAt    time.Time // latest shot awaiting a hit, zero if none
}

func newBot(id int, cfg config, st *stats, rec *recorder, logger *log.Logger) *bot {
	return &bot{
		id:       id,
		deviceID: fmt.Sprintf("%s-%04d", cfg.devicePrefix, id),
		cfg:      cfg,
		stats:    st,
		rec:      rec,
		log:      logger,
		data:     make(chan *nakama.MatchDataMsg, 64),
	}
}

// run authenticates once, then plays matches back to back until ctx is done.
func (b *bot) run(ctx context.Context) {
	cl := nakama.New(
		nakama.WithURL(b.cfg.url),
		nakama.WithServerKey(b.cfg.serverKey),
	)

	if err := b.authenticate(ctx, cl); err != nil {
		b.stats.errors.Add(1)
		b.logf("authenticate failed: %v", err)
		return
	}

	// The server renames accounts in an after-authenticate hook, so read the
	// account back rather than trusting the name on the session.
	start := time.Now()
	account, err := cl.Account(ctx)
	b.rec.observe(ctx, opAccount, start, err)
	if err != nil {
		b.stats.errors.Add(1)
		b.logf("account lookup failed: %v", err)
		return
	}
	b.userID, b.username = account.User.Id, account.User.Username

	start = time.Now()
	conn, err := cl.NewConn(ctx, nakama.WithConnFormat("json"))
	b.rec.observe(ctx, opConnect, start, err)
	if err != nil {
		b.stats.errors.Add(1)
		b.logf("socket connect failed: %v", err)
		return
	}
	b.rec.connected.Add(1)
	defer b.rec.connected.Add(-1)
	defer conn.Close()

	// Set once for the lifetime of the connection rather than per match, so
	// there's no window where an incoming message has no handler.
	conn.MatchDataHandler = func(_ context.Context, msg *nakama.MatchDataMsg) {
		select {
		case b.data <- msg:
		default: // never let a slow play loop stall the socket
		}
	}

	b.logf("connected as %s (%s)", b.username, b.deviceID)

	for ctx.Err() == nil {
		if err := b.playMatch(ctx, cl, conn); err != nil {
			if ctx.Err() != nil {
				return
			}
			b.stats.errors.Add(1)
			b.logf("match failed: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(b.cfg.retryDelay):
			}
		}
	}
}

func (b *bot) playMatch(ctx context.Context, cl *nakama.Client, conn *nakama.Conn) error {
	start := time.Now()
	var found findMatchResponse
	err := nakama.Rpc(rpcFindMatch, nil, &found).Do(ctx, cl)
	b.rec.observe(ctx, opFindMatch, start, err)
	if err != nil {
		return fmt.Errorf("find_match: %w", err)
	}

	// Drop anything left over from the previous match before joining.
	b.drain()

	start = time.Now()
	_, err = conn.MatchJoin(ctx, found.MatchID, nil)
	b.rec.observe(ctx, opJoin, start, err)
	if err != nil {
		return fmt.Errorf("join %s: %w", found.MatchID, err)
	}
	b.stats.matches.Add(1)
	if found.Created {
		b.stats.created.Add(1)
	}
	b.rec.inMatch.Add(1)
	defer b.rec.inMatch.Add(-1)
	b.logf("joined %s (created: %t)", found.MatchID, found.Created)

	defer func() {
		// Best effort, and bounded: this still has to run when ctx is already
		// cancelled, but an unbounded wait here hangs shutdown if the socket
		// is going away and the leave is never acknowledged.
		leaveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.MatchLeave(leaveCtx, found.MatchID)
	}()

	// The server enforces a shoot cooldown, so firing faster than it just
	// burns messages that get discarded.
	shoot := time.NewTicker(b.cfg.shootInterval)
	defer shoot.Stop()

	deadline := time.NewTimer(b.cfg.matchTimeout)
	defer deadline.Stop()

	track := matchTrack{oppHealth: -1}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-deadline.C:
			b.stats.timeouts.Add(1)
			b.logf("match %s timed out after %s, leaving", found.MatchID, b.cfg.matchTimeout)
			return nil

		case <-shoot.C:
			sent := time.Now()
			err := conn.MatchDataSend(ctx, found.MatchID, opCodeShoot, []byte("{}"), true)
			b.rec.observe(ctx, opShoot, sent, err)
			if err != nil {
				return fmt.Errorf("shoot: %w", err)
			}
			b.stats.shots.Add(1)
			// Shots before the match starts are ignored server side, so they
			// can never produce a hit to time.
			if track.started {
				track.shotAt = sent
			}

		case msg := <-b.data:
			done, err := b.handle(msg, &track)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// handle reports whether the match has finished.
func (b *bot) handle(msg *nakama.MatchDataMsg, track *matchTrack) (bool, error) {
	switch msg.OpCode {
	case opCodeStateSync:
		var state matchStateMsg
		if err := json.Unmarshal(msg.Data, &state); err != nil {
			return false, fmt.Errorf("decode state sync: %w", err)
		}
		track.started = state.Started

		for _, p := range state.Players {
			if b.cfg.verbose {
				b.logf("  %s: %d HP", p.Username, p.Health)
			}
			if p.UserID == b.userID {
				continue
			}
			// With two players only our shots can lower the opponent's health,
			// so a drop closes the round trip for our latest shot. A single hit
			// deals at most 10, which keeps a forfeit (health set straight to
			// zero) from being mistaken for one.
			drop := track.oppHealth - p.Health
			if track.oppHealth >= 0 && drop >= 1 && drop <= 10 && !track.shotAt.IsZero() {
				b.rec.sample(opShotToHit, time.Since(track.shotAt))
				track.shotAt = time.Time{}
			}
			track.oppHealth = p.Health
		}

	case opCodeMatchOver:
		var over matchOverMsg
		if err := json.Unmarshal(msg.Data, &over); err != nil {
			return false, fmt.Errorf("decode match over: %w", err)
		}
		switch {
		case over.WinnerUsername == "":
			b.logf("match over: draw")
		default:
			b.logf("match over: %s wins", over.WinnerUsername)
		}
		if over.WinnerID != "" && over.WinnerID == b.userID {
			b.stats.wins.Add(1)
		}
		return true, nil
	}

	return false, nil
}

func (b *bot) drain() {
	for {
		select {
		case <-b.data:
		default:
			return
		}
	}
}

func (b *bot) logf(format string, args ...any) {
	if b.cfg.quiet {
		return
	}
	b.log.Printf("[bot %03d] "+format, append([]any{b.id}, args...)...)
}

// authenticate retries on failure. A fleet starting at once can exhaust the
// accept path and get refused at the TCP layer before Nakama sees the request;
// giving up there would silently shrink the fleet mid-run and make every later
// number describe the harness rather than the server.
func (b *bot) authenticate(ctx context.Context, cl *nakama.Client) error {
	backoff := b.cfg.authBackoff

	for attempt := 1; ; attempt++ {
		start := time.Now()
		err := cl.AuthenticateDevice(ctx, b.deviceID, true, "")
		b.rec.observe(ctx, opAuth, start, err)
		if err == nil {
			if attempt > 1 {
				b.logf("authenticated after %d attempts", attempt)
			}
			return nil
		}
		if attempt >= b.cfg.authAttempts || ctx.Err() != nil {
			return err
		}

		b.stats.authRetries.Add(1)

		// Jittered, or every bot refused in the same burst comes back in the
		// same burst and gets refused again.
		wait := backoff + time.Duration(rand.Int63n(int64(backoff)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff *= 2
	}
}
