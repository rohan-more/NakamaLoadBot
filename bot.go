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
	matches  atomic.Int64
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
	log      *log.Logger

	// Resolved after authenticating; used to tell our own wins apart.
	userID   string
	username string

	// Match data arrives on the connection's reader goroutine; the play loop
	// consumes it from here so handlers never block the socket.
	data chan *nakama.MatchDataMsg
}

func newBot(id int, cfg config, st *stats, logger *log.Logger) *bot {
	return &bot{
		id:       id,
		deviceID: fmt.Sprintf("%s-%04d", cfg.devicePrefix, id),
		cfg:      cfg,
		stats:    st,
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
	account, err := cl.Account(ctx)
	if err != nil {
		b.stats.errors.Add(1)
		b.logf("account lookup failed: %v", err)
		return
	}
	b.userID, b.username = account.User.Id, account.User.Username

	conn, err := cl.NewConn(ctx, nakama.WithConnFormat("json"))
	if err != nil {
		b.stats.errors.Add(1)
		b.logf("socket connect failed: %v", err)
		return
	}
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
	var found findMatchResponse
	if err := nakama.Rpc(rpcFindMatch, nil, &found).Do(ctx, cl); err != nil {
		return fmt.Errorf("find_match: %w", err)
	}

	// Drop anything left over from the previous match before joining.
	b.drain()

	if _, err := conn.MatchJoin(ctx, found.MatchID, nil); err != nil {
		return fmt.Errorf("join %s: %w", found.MatchID, err)
	}
	b.stats.matches.Add(1)
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-deadline.C:
			b.stats.timeouts.Add(1)
			b.logf("match %s timed out after %s, leaving", found.MatchID, b.cfg.matchTimeout)
			return nil

		case <-shoot.C:
			if err := conn.MatchDataSend(ctx, found.MatchID, opCodeShoot, []byte("{}"), true); err != nil {
				return fmt.Errorf("shoot: %w", err)
			}
			b.stats.shots.Add(1)

		case msg := <-b.data:
			done, err := b.handle(msg)
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
func (b *bot) handle(msg *nakama.MatchDataMsg) (bool, error) {
	switch msg.OpCode {
	case opCodeStateSync:
		if !b.cfg.verbose {
			return false, nil
		}
		var state matchStateMsg
		if err := json.Unmarshal(msg.Data, &state); err != nil {
			return false, fmt.Errorf("decode state sync: %w", err)
		}
		for _, p := range state.Players {
			b.logf("  %s: %d HP", p.Username, p.Health)
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
	b.log.Printf("[bot %03d] "+format, append([]any{b.id}, args...)...)
}

// authenticate retries on failure. A fleet starting at once can exhaust the
// accept path and get refused at the TCP layer before Nakama sees the request;
// giving up there would silently shrink the fleet mid-run and make every later
// number describe the harness rather than the server.
func (b *bot) authenticate(ctx context.Context, cl *nakama.Client) error {
	backoff := b.cfg.authBackoff

	for attempt := 1; ; attempt++ {
		err := cl.AuthenticateDevice(ctx, b.deviceID, true, "")
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
