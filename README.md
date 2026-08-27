# netrate

A terminal download-rate meter. Measures sustained throughput over several
parallel TCP connections and tells you how *steady* the link is, not only how
fast it looked for a moment.

```
╭──────────────────────────────────────────────────╮
│  netrate                                ✓ stable │
│                                                  │
│  623.5 Mbps      median of 30 samples · CV 0.2%  │
│                                                  │
│   800┤·  ·  ·  ·  ·  ·  ·  ·  ·  ·  ·  ·  ·  ·   │
│      │           ▂▄▃▁▁▃▅▅▃▁▂▄▅▄▂▁▃               │
│   600┤·  · ▁▅▇▆▆██████████████████  ·  ·  ·  ·   │
│      │    ▃███████████████████████               │
│   400┤· ▂▇████████████████████████  ·  ·  ·  ·   │
│      │ ▃██████████████████████████               │
│   200┤·███████████████████████████  ·  ·  ·  ·   │
│      │▇███████████████████████████               │
│     0┼╌╌╌╌╌╌╌───────┬─────────────┬────────────  │
│       0s            2s            4s             │
│                                                  │
│  p50 623.5 Mbps              trimmed 623.6 Mbps  │
│  min 478.1 Mbps                  max 876.2 Mbps  │
│  downloaded 371.3 MiB             duration 5.4s  │
│  streams 6                            CV 0.2%    │
╰──────────────────────────────────────────────────╯
  r run again  ·  q quit
```

The dashed stretch on the x axis is the warm-up, discarded before any statistic
is computed.

## Install

```sh
go install github.com/grananda/netrate@latest
```

Or download a binary for linux, macOS or Windows (amd64/arm64) from the
[releases page](https://github.com/grananda/netrate/releases).

## Use

```sh
netrate                          # full screen chart
netrate -plain                   # one report, then exit
netrate -plain -duration 30s     # measure for longer
netrate -streams 12              # more parallel connections
```

`-plain` keeps stdout to the report alone and sends progress to stderr, so it
composes in a pipe:

```
$ netrate -plain > result.txt
Download   623.5 Mbps  (median of 30 samples, 6 streams)
Trimmed    623.6 Mbps
Range      478.1 Mbps – 876.2 Mbps
CV         0.2% (stable)
Volume     371.3 MiB in 5.4s
Stopped    early at 5.4s of the 12.0s budget (the reading settled)
```

## Reading the output

**CV** — coefficient of variation: the standard deviation as a percentage of
the mean. It measures consistency, not speed. Below 4% the reading is called
stable. The extremes are trimmed first, so one scheduling hiccup cannot condemn
an otherwise steady run.

**Trimmed** — the mean after dropping the fastest and slowest 10%.

**Warm-up** — the first 2.5s are discarded. TCP spends them in slow start, and
counting them would understate a fast link.

**"Stopped early"** — the run ended before its budget because the reading had
settled: both the last two seconds *and* the run as a whole were inside the
stability threshold. Measuring longer would not have changed the answer. Use
`-minrun` equal to `-duration` to always spend the whole budget.

## Why a duration and not a file size

A fixed-size file finishes in a second on fibre — too little time to leave TCP
slow start — and takes minutes on a slow link. Measuring for a fixed *duration*
gives the same quality of answer on both.

## Flags

```
-url       string     large, incompressible file to download
-streams   int        concurrent TCP connections (default 6)
-duration  duration   maximum length of the download phase (default 14s)
-warmup    duration   leading window discarded from the statistics (default 2.5s)
-interval  duration   sampling period for instantaneous speed (default 250ms)
-minrun    duration   keep measuring at least this long before a settled reading
                      may stop the run (default: 45% of -duration)
-plain                no interface: print the result and exit
-version              print the version and exit
```

## Exit codes

| Code | Meaning |
|-----:|---------|
| 0 | measured |
| 1 | the measurement failed |
| 2 | the flags do not describe a runnable measurement |
| 130 | interrupted |

## Releases

`VERSION` is the source of truth. Bump it in a pull request; merging to `main`
tags and publishes. Leaving it alone publishes nothing.

## Licence

[MIT](LICENSE)
