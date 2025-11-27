# Measuring performance

What the gateway adds is measured, not estimated. The method keeps everything the same except the thing being measured, and records enough about each run to tell a clean measurement from a contended one.

## Running it

```bash
make dev-up          # Postgres and Redis
./scripts/bench.sh   # About five minutes
```

Results go to `bench/results/<date>-<commit>/`: one JSON file per run, the conditions of the run, a CPU profile of tenant mode, and a `README.md` with the tables. Settings are `BENCH_CONCURRENCY` (16), `BENCH_DURATION` (20s), `BENCH_WARMUP` (5s), and `BENCH_STREAMS` (200).

## What's compared

Every run uses the same mock, payload, concurrency, duration, and warm-up. Only where the requests go changes.

| Run | Path |
|---|---|
| `direct` | Straight to the mock |
| `dev` | Through the gateway in development mode: validation, routing, and the adapter, with no authentication store, limits, or accounting |
| `tenant` | Through the gateway in tenant mode: key lookup, team limits, reservation, dispatch, and settlement in Postgres |

The difference between `direct` and `dev` is what proxying costs. The difference between `dev` and `tenant` is what authentication, limits, and accounting cost. Each comparison is run for non-streaming requests, and for streams, where time to first byte matters most.

The script also measures:

| Measurement | How |
|---|---|
| Failover latency | Every request first fails on a route answering 500, then succeeds on the next, compared with going straight to the working route |
| Memory and goroutines per active stream | The gateway's own heap and goroutine counts, idle and with 200 slow streams open |
| Fairness | A provider allowing 8 attempts at once, a noisy team keeping 64 requests in flight, and a quiet team keeping 2: the quiet team's latency alone, then beside the noisy one |
| Where tenant mode spends CPU | A 10-second CPU profile during the tenant run |
| Liability accuracy | Every attempt's reservation against its charge, how many were charged conservatively or above their reservation, and whether every budget agrees with the ledger |

## The harness

`llmgate-bench` sends requests from a fixed number of workers for a fixed time, after a warm-up whose requests are sent but not counted. For each response it records the time to the first byte of the body, which for a stream is its first event, and the time to read the whole response. It reports nearest-rank percentiles, throughput, and errors by status, as JSON. `llmgate-bench compare` puts several runs in a table, with what each adds to the first. The API key comes from `LLMGATE_BENCH_KEY` and never appears in the output.

## Conditions

Each run records the commit, the Go version, the operating system, the CPU, its cores and memory, how Postgres and Redis were run, the workload, the load average before it started, and the CPU other processes used during it, sampled every 10 seconds. The benchmark loads the machine itself, so the load average during the run says nothing about other work. A run is labeled **contended** if the load average before it exceeded a quarter of the cores, or other processes averaged more than one core, and its report says so at the top. A contended run isn't presented as a measurement of the gateway.

Postgres and Redis run in containers, which on macOS means inside a VM, so tenant mode's numbers include that network hop. They are a property of this setup, not of a production deployment.

## Published results

None yet. The runs so far were on a machine shared with other work, and every one was contended.
