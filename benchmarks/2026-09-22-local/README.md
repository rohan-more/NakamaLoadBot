# Ladder run, 2026-09-22, single machine

Produced with `cmd/ladder` using its defaults: modes `naive,serialized,seats`,
bots `20,100,500`, ramps `100ms,0s`, 60 seconds per run.

- Server: NakamaLoadServer `19174ba`, Nakama 3.26.0, one node plus Postgres 12,
  in Docker Desktop on Windows.
- Bots: NakamaLoadBot `69573e5`, run natively on the same machine and reaching
  the server through Docker Desktop's port forwarding.

Because bots and server share one host, latencies are localhost figures and
do not include real network time. At 500 bots with no stagger some
connections are refused by the port forwarding before Nakama sees them,
which shows up as auth retries rather than server errors.

`summary.md` is the table below. `summary.csv` has every column, and each
`<mode>-<ramp>-<bots>.json` is that run's full bot report, including the
once-per-second connected and in-match series.

"Games started" and "orphaned matches" come from the server log for that run.
"Joins" is the bot's own count, which is not the same thing: a bot that joins
an empty match of its own counts as a join even though no game starts.

| mode | ramp | bots | games started | orphaned matches | joins | join fail | errors | find_match p95 | join p95 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| naive | 100ms | 20 | 18 | 3 | 40 | 9.1% | 4 | 2.00 ms | 1.80 ms |
| naive | 100ms | 100 | 97 | 3 | 198 | 12.0% | 27 | 2.36 ms | 1.66 ms |
| naive | 100ms | 500 | 326 | 3 | 655 | 6.0% | 42 | 1.99 ms | 1.68 ms |
| naive | 0ms | 20 | 0 | 20 | 20 | 0.0% | 0 | 5.84 ms | 4.85 ms |
| naive | 0ms | 100 | 0 | 100 | 100 | 0.0% | 0 | 28.96 ms | 28.63 ms |
| naive | 0ms | 500 | 180 | 190 | 560 | 15.0% | 162 | 41.58 ms | 52.53 ms |
| serialized | 100ms | 20 | 20 | 0 | 40 | 0.0% | 0 | 1.86 ms | 1.79 ms |
| serialized | 100ms | 100 | 100 | 0 | 200 | 0.0% | 0 | 2.45 ms | 1.63 ms |
| serialized | 100ms | 500 | 325 | 0 | 651 | 2.5% | 17 | 1.92 ms | 1.57 ms |
| serialized | 0ms | 20 | 19 | 0 | 40 | 47.4% | 36 | 5.96 ms | 5.66 ms |
| serialized | 0ms | 100 | 96 | 1 | 198 | 52.3% | 217 | 15.30 ms | 3.38 ms |
| serialized | 0ms | 500 | 480 | 1 | 985 | 47.8% | 900 | 51.08 ms | 3.14 ms |
| seats | 100ms | 20 | 20 | 0 | 40 | 0.0% | 0 | 2.85 ms | 2.02 ms |
| seats | 100ms | 100 | 99 | 0 | 198 | 0.0% | 0 | 2.73 ms | 1.70 ms |
| seats | 100ms | 500 | 331 | 0 | 662 | 0.0% | 0 | 2.03 ms | 1.73 ms |
| seats | 0ms | 20 | 20 | 0 | 40 | 0.0% | 0 | 6.14 ms | 4.98 ms |
| seats | 0ms | 100 | 100 | 0 | 200 | 0.0% | 0 | 33.83 ms | 9.89 ms |
| seats | 0ms | 500 | 496 | 1 | 993 | 0.0% | 3 | 94.66 ms | 13.73 ms |
