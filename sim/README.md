# sim — SingR traffic accounting simulation

This folder is a self-contained, in-process simulation that proves whether
SingR's node-side byte accounting matches what gets posted to SSPanel
`/mod_mu/users/traffic`. It exists because production reports stable
~100× over-counting **even when every report HTTP call succeeds** in the
log, which rules out retry-class causes.

## What the sim covers

`sim_test.go` holds three tests:

1. **`TestCounterByteSemantics`** — wraps a `net.Pipe` with the same
   `bufio.NewInt64CounterConn` call `poet.RoutedConnection` uses in
   production, pumps 1 MiB in each direction, asserts the atomic counters
   on the user grew by exactly 1 MiB. Catches any unit/encoding problem
   in the byte-counter wrapper itself.
2. **`TestEndToEndReportMatchesGroundTruth`** — spins up a fake SSPanel
   (`fake_panel.go`), constructs a real `sspanel.APIClient` pointing at
   it, sets up a real `controller.Authenticator` with one `u<UID>` user,
   pumps known bytes across **multiple** wrapped connections sharing the
   user's pointers, replicates `userInfoMonitor`'s inner loop, and
   asserts the bytes the fake panel receives match the bytes pumped
   exactly. Catches counter→report wiring errors.
3. **`TestRepeatedCyclesNoInflation`** — repeats the report cycle five
   times back-to-back and asserts the panel's accumulated total equals
   `cycles * perCycleBytes`. Catches any per-cycle double-counting.

If all three pass with `bytes_pumped == bytes_panel_received`, the
node-side Go path is exonerated and the 100× must come from outside the
SingR Go process (panel-side `traffic_rate`, panel insert/display logic,
multiple SingR instances reporting the same `node_id`, or environment).

## Running

```sh
GOCACHE=$(pwd)/.cache/go-build go test ./sim -v
```

To get the gated `[TRAFFIC]` debug logs from `poet/poet.go`,
`poet/controller/controller.go`, and `poet/api/sspanel/sspanel.go`:

```sh
SINGR_TRAFFIC_DEBUG=1 GOCACHE=$(pwd)/.cache/go-build go test ./sim -v
```

The `[TRAFFIC]` logs print:
- per-conn wrap and per-conn close-time `(read, write)` byte totals
  (`poet/poet.go`),
- per-user `(UID, sent, recv)` snapshot just before reporting and the
  Σ across users (`poet/controller/controller.go`),
- the actual JSON body posted to `/mod_mu/users/traffic`
  (`poet/api/sspanel/sspanel.go`).

These logs are off (`if trafficDebug`/`if debug`) when the env var is unset.
They are intended as temporary diagnostic instrumentation — once the
investigation is closed, they can be removed (or left as a permanent
debug switch, which is a one-line decision).

## What this sim does NOT cover

- The **AnyTLS protocol layer** itself (sing-anytls service framing,
  multiplexer, padding). Counter bytes ARE counted post-decryption /
  post-deframing per the analysis in `AGENTS.md`, but bringing up a
  full AnyTLS client+server in-process is much heavier than what's
  needed to answer the present question.
- The **periodic `userInfoMonitor` task scheduling**. The inner loop is
  replicated verbatim from `controller.go`; only the `time.Periodic`
  wrapping is skipped.
- The **panel-side accounting** (insert/display, `traffic_rate`).
  That is exactly what this sim is designed to rule OUT or rule IN as
  the cause of inflation.
