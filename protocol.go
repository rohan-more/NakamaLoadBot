package main

// Mirrors the opcodes and RPC ids defined by the shooter match handler in
// NakamaLoadServer. Keep these in sync with match_handler.go there.
const (
	opCodeShoot     = 1
	opCodeStateSync = 2
	opCodeMatchOver = 3

	rpcFindMatch = "find_match"
)

type findMatchResponse struct {
	MatchID string `json:"match_id"`
	Created bool   `json:"created"`
}

type playerState struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Health   int    `json:"health"`
}

type matchStateMsg struct {
	Players []playerState `json:"players"`
	Started bool          `json:"started"`
}

type matchOverMsg struct {
	WinnerID       string `json:"winner_id"`
	WinnerUsername string `json:"winner_username"`
}
