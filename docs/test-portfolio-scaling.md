# Race and volume qualification

The previous local gate spent 1,723.889 seconds on the exhaustive migration
transport matrix. A separate ordinary portable race shard exceeded its
30-minute process deadline while running the 10,000-upload lifecycle; that
individual test had run for 3m7s after earlier tests consumed the remaining
time. No data race was reported. This is repeated work and instrumentation
cost, not evidence that a larger arbitrary sample proves more concurrency.

Measured transport inventory is 3,515 cases, including eighteen terminal
probes. There are 2,329 GETs, 58 HEADs, 198 LISTs, 636 PUTs, 275 DELETEs, and
one upload abort. Each offset initializes fresh state/file backends, attempts
the upgrade, and independently resumes it. The ordinary owning migration gate
retains this full inventory and the complete epoch/profile/crypto matrix.

Under `-race`, mutable-root/gate/writer/lease effects remain selected, while
immutable page/node/pack/manifest publication or deletion and reads are
represented by their first/last occurrence in each migration phase and record
family. Unknown operation classes fail closed. A selector test proves critical
mutations cannot disappear and preserves both ends of a read class; separately
named durable-boundary and two-to-eight replica schedules remain unchanged.
This deliberately supersedes repetition at every immutable offset under the
race detector, not the exhaustive ordinary fault gate.

The namespace lifecycle sample is 1,024 items; a separate geometric test runs
64/256/1,024 and requires exactly four state requests, zero file traffic, and
bounded metadata/allocation growth. Transfer/call shape is checked throughout.
Allocation slope is compared between the 256- and 1,024-item multi-page samples;
the 64-item compact regime has different fixed and race-instrumentation costs
and is reported separately. The per-item growth allowance permits at most a
doubling within the multi-page regime and rejects quadratic/exponential
amplification. Allocation
measurements are collected around a serial foreground mutation, outside setup;
timing is not used as an assertion. Observed transfer bytes are
33,604 / 112,582 / 438,724. Representative allocated bytes are about
15.1 / 112.9 / 448.2 MB, with normal runtime variation.

Upload admission uses 100 / 1,001 / 2,001 real sessions. File calls equal
cardinality exactly; state calls are 4 / 5 / 6, following the implemented
1,000-item progress boundary. Observed metadata bytes are
49,988 / 566,643 / 1,130,341. Completion and cancellation use 2,001 items,
covering two full segments and a partial third; restart and partial-progress
denial tests remain mandatory. Namespace publication, replay, last-item denial,
restore, copy/move, and listing preserve their original semantic assertions at
the new selected size. Input-limit tests retain the 10,000-item public contract.

| Guarantee | Replacement evidence |
|---|---|
| Every provider interruption remains recoverable | Uninstrumented full migration packages and exhaustive offset matrix remain mandatory. |
| Shared-memory and stale-worker safety | All mutable transitions plus phase/family endpoints run under `-race`; existing explicit replica/concurrent schedules remain. |
| Every durable schema/profile remains supported | Fixtures, ordered suffix, independent authority verification, cryptographic oracle, and 98% migration coverage are unchanged. |
| Volume does not amplify provider work superlinearly | Geometric namespace requests stay constant; upload requests are exactly linear and state work increases by segment. |
| Foreground metadata/memory work remains bounded | Geometric byte and allocation growth assertions supplement provider counts. |
| Atomic complete visibility, replay, and denial | Existing lifecycle assertions are retained at representation-crossing sample sizes. |
| Historical economics remain auditable | Old fixtures are untouched; explicit supersession maps bind current workload names to a new measured append-only delta. |

Rejected alternatives: raising timeouts preserves avoidable cost; silently
skipping tests gives no replacement proof; random sampling can miss unique
state boundaries; keeping a single smaller cardinality cannot reveal growth.
The current portfolio uses semantic classes plus a measured curve. Future
regressions that change tree/pack/segment representation require new boundary
samples and updated evidence, not a silently increased tolerance.

The largest former-sample economics scenarios are explicitly marked historical.
They are retained comparative measurements, not claims that the current gate
executes those cardinalities. Current budget-catalog completeness still requires
every replacement ratchet to be referenced by an executable test, and every
superseded fixture to remain present.
