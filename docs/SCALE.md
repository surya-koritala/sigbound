# Extended-scale numbers (1024–4096 agents)

The [README benchmark table](../README.md#benchmarks) stops at 512 agents — a
laptop-friendly, reproduce-in-a-minute size. This page carries the overlay fold
out to **4096 agents** on two machines, states the observed scaling exponent,
and names where the cost would first turn super-linear if it ever did. It has
not, through 4096.

All points below are the real workload: N agents commit to one 2000-file repo
in parallel on disjoint file sets, their branches are folded by the `overlay`
strategy, and **correctness is verified on every point** (the folded tree holds
every landed change — `treeHasAllLanded`, the same gate `sigbench` exits
non-zero on). Reproduce any row with:

```bash
go run ./cmd/sigbench -agents 4096 -files 2000 -strategy overlay -runs 3 -warmup 1
```

## Measured

**EC2 c7i.4xlarge — 16-core Xeon 8488C, Amazon Linux 2023** (runs=3, warmup=1):

| Agents | overlay | sequential `git merge` | speedup | overlay ms/agent |
|-------:|--------:|-----------------------:|--------:|-----------------:|
| 64   | 0.11 s | 1.7 s  | 15× | 1.77 |
| 512  | 0.80 s | 13.1 s | 16× | 1.56 |
| 1024 | 1.58 s | 32.1 s | 20× | 1.54 |
| 2048 | 3.15 s | 88.6 s | 28× | 1.54 |
| 4096 | 6.61 s | (not run) | — | 1.61 |

**M-series laptop (10-core), macOS** (runs=3, warmup=1):

| Agents | overlay | sequential `git merge` | speedup | overlay ms/agent |
|-------:|--------:|-----------------------:|--------:|-----------------:|
| 1024 | 2.42 s  | 80.9 s  | 33× | 2.36 |
| 2048 | 5.15 s  | 206.8 s | 40× | 2.51 |
| 4096 | 11.94 s | (not run) | — | 2.92 |

Two machines because the point isn't a single "fastest" number: a 16-core
server and a 10-core laptop bound the fold from both ends. The server wins at
scale (3.15 s vs 5.15 s at 2048) — more cores fold more groups at once once N is
large — and this is measured hardware, not a spec sheet.

## Scaling exponent

Fitting `t = a·N^b` (least squares on log time vs log agents) to the overlay
column:

- **Linux, 64–4096: b ≈ 0.98.** Steady region 512–4096: **b ≈ 1.01.** Flat.
  Per-agent cost holds at ~1.54–1.61 ms across a 64× range in N. Doubling N
  doubles the time (local exponents 0.98, 0.99, 1.07 across the last three
  doublings) — the definition of linear.
- **macOS, 1024–4096: b ≈ 1.15.** A mild upward tilt (ms/agent 2.36 → 2.92 over
  the top two doublings), consistent with a smaller core count and more GC/memory
  pressure on the laptop. It is a drift, not a knee: b = 1.15 is still far below
  the b ≥ 2 a serial quadratic phase would show.

**The overlay fold is linear in the number of agents through 4096 branches.**
Sequential `git merge` is clearly super-linear over the same range (26 → 43
ms/agent from 64 → 2048 on the Xeon), which is why the speedup *grows* with
scale rather than staying flat: 15× → 28× on Linux, 33× → 40× on macOS.

## Where the first bottleneck would be

`sigbench` reports end-to-end fold time, not a per-phase flamegraph, so the
evidence here is the flat per-agent curve above, read against the fold's
structure. Every phase that touches all N branches is deliberately a *single*
batched process, not a per-branch fork — that design is why the line stays
straight. The candidates for the first phase to turn super-linear, in the order
they'd bite:

- **The `update-index --index-info` batch in `OverlayTrees`**
  ([`internal/gitx/plumbing.go`](../internal/gitx/plumbing.go)). The combine
  phase gathers every group head's changed entries in one `git diff-tree
  --stdin` run, applies them through **one** `update-index --index-info`, and
  emits the union with one `write-tree`. Cost is proportional to *total changed
  paths*, not to N² — but it is a single serialized index write, so it is the
  most likely first ceiling once the entry stream gets large enough that git's
  index rewrite dominates. Flat through 4096.

- **The partition union-find** ([`cell/occ.go`](../cell/occ.go)). Pure
  in-memory: an inverted path→branch map feeding a path-halving, union-by-rank
  disjoint-set — effectively linear in total paths. The map itself grows with
  the number of distinct paths across all branches; at 4096 disjoint agents it
  is not visible in the timing.

- **The `update-ref` landing transaction**
  ([`internal/gitx/plumbing.go`](../internal/gitx/plumbing.go), `UpdateRefs`).
  One `git update-ref --stdin` applies the whole batch atomically. Linear in the
  ref count; a non-issue for a single final land, listed because it is the other
  all-branches serial point.

- **`git bundle` export/import proportionality**
  ([`cell/bundle.go`](../cell/bundle.go)). Off the overlay hot path — it is the
  distributed-run transport — but built the same way (one `cat-file
  --batch-check` to resolve, one `bundle create`, one `update-ref --stdin` to
  map). Whether `git bundle` itself stays proportional past ~2k branches is
  **not exercised by the overlay sweep** and remains an open follow-up.

No super-linear bottleneck has appeared through 4096 agents. Naming the actual
first one — rather than the candidate list above — means pushing the sweep to
8k–16k, where a knee, if one exists, should finally show. That probe is future
work.

## Regression guard

The nightly workflow ([`.github/workflows/nightly.yml`](../.github/workflows/nightly.yml))
runs one `overlay` fold at **2048 agents** with the correctness check on and the
exit code authoritative, so a scale regression — a fold that goes wrong or blows
up at the top of the published range — fails the nightly instead of going
unnoticed.
