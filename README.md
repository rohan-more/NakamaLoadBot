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
| `-v` | `false` | Log every state sync |

Device ids are `<prefix>-<0-padded index>`, so the same accounts are reused
across runs. Change `-device-prefix` to get a fresh set.

`-shoot-interval` defaults above the server's shoot cooldown (20 ticks at 10
ticks/sec, so 2s); firing faster only produces messages the server discards.

## Keeping in sync with the server

`protocol.go` mirrors the opcodes and RPC ids from the server's
`match_handler.go`. If those change, update them here too.
