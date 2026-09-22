# NakamaLoadBot

Headless Go bots that play the authoritative shooter match served by
[NakamaLoadServer](https://github.com/rohan-more/NakamaLoadServer), for load
testing it without running Unity clients.

Each bot authenticates with its own device id, calls the `find_match` RPC,
joins the match it gets back, shoots on a timer, and reads the match state and
match over broadcasts. When a match ends it finds another one, until the run
is stopped. Because matchmaking pairs bots server side, running an even number
of bots gets them playing against each other.

## Running

With Go installed:

```bash
go run . -bots 10
```

Without a local Go toolchain, build a Windows binary in a container:

```bash
docker run --rm -v "D:/Work/NakamaLoadBot:/src" -w /src -e GOOS=windows -e GOARCH=amd64 golang:1.23 go build -o loadbot.exe .
```

Then run `loadbot.exe` against a server started with `docker compose up -d`.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-bots` | `2` | Number of bots to run |
| `-url` | `http://127.0.0.1:7350` | Nakama server URL |
| `-key` | `defaultkey` | Nakama server key |
| `-device-prefix` | `loadbot` | Prefix for generated device ids |
| `-shoot-interval` | `2.5s` | Delay between shots |
| `-match-timeout` | `90s` | Give up on a match that never finishes |
| `-retry-delay` | `2s` | Delay before retrying after a failed match |
| `-duration` | `0` | How long to run; `0` runs until Ctrl-C |
| `-ramp-delay` | `100ms` | Delay between starting each bot |
| `-auth-attempts` | `5` | Attempts to authenticate before giving up on a bot |
| `-auth-backoff` | `250ms` | Initial backoff between attempts; doubles and is jittered |
| `-sample-every` | `1s` | How often connected and in-match bots are sampled |
| `-out` | none | Write a JSON report of the run to this path |
| `-label` | none | Free-form label stored in the JSON report |
| `-quiet` | `false` | Print only the summary, not per-bot logs |
| `-v` | `false` | Log every state sync |

Device ids are `<prefix>-<0-padded index>`, so the same accounts are reused
across runs. Change `-device-prefix` to get a fresh set.

`-shoot-interval` defaults above the server's shoot cooldown (20 ticks at 10
ticks/sec, so 2s); firing faster only produces messages the server discards.

## Metrics

Every run ends with a summary table, and `-out` writes the same data as JSON
along with a once-per-second series of connected and in-match bots.

| Op | What is timed |
| --- | --- |
| `auth` | Device authentication, one sample per attempt including retries |
| `account` | Reading the account back after authenticating |
| `connect` | Opening the realtime socket |
| `find_match` | The `find_match` RPC |
| `join` | Joining the returned match over the socket |
| `shoot_send` | Writing a shot to the socket |
| `shot_to_hit` | From sending a shot to receiving the state sync in which the opponent's health dropped |

Each op reports attempts, failure rate, and p50/p95/p99/max latency.
Percentiles are taken over successful attempts only. Errors caused by the run
itself shutting down are not counted.

`shot_to_hit` includes up to one server tick of quantisation: at 10 ticks per
second a shot is only applied, and its result only broadcast, on a tick
boundary. It is attributable because with two players only your own shots can
lower your opponent's health. A drop of more than 10 is ignored so that a
forfeit is not mistaken for a hit.

`peak connected` counts bots with an open socket, which is not the same as
bots launched: a bot that cannot authenticate never connects.

## Keeping in sync with the server

`protocol.go` mirrors the opcodes and RPC ids from the server's
`match_handler.go`. If those change, update them here too.
