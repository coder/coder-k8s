---
name: verify-aigatewaykey-model
description: Model-check the CoderAIGatewayKey key lifecycle (issue, Secret write, rotate, retire, orphan sweep, delete) with TLA+ against facts read from the Go source. Use it whenever a change touches internal/controller/coderaigatewaykey_*.go or writeOwnedSecret/getOwnedSecret in internal/controller/owned_secret.go, or changes the settle window, the write age, the marker write, status or Secret write concurrency, or the finalizer order.
---

# Verify the CoderAIGatewayKey key lifecycle model

A CoderAIGatewayKey keeps one full-trust Coder AI Gateway key in a Secret (`docs/how-to/manage-ai-gateway-keys.md`). Its controller works across reconciles that can stop at any step, a cache that lags, and requests that land after the controller gave up on them. Unit tests rarely reach those interleavings. This check explores all of them inside a bound.

## The rules

1. `SecretKeyAlive`: while the object exists, the output Secret never holds a key that Coder no longer has, unless a person deleted that key in Coder.
   Harm when broken: every AI Gateway that reads the Secret loses access at its next connection, and the operator sees nothing wrong.
2. `NoLeakAfterRelease`: when the operator removes the finalizer `coder.com/ai-gateway-key-cleanup`, Coder has no key of the object, and no create of the object can still commit.
   Harm when broken: a full-trust gateway key stays valid in Coder after its object is gone. Nobody tracks it, and nothing deletes it.

## What a pass means

The model is `hack/formal/aigatewaykey/AIGatewayKey.tla`. A pass covers that model inside its bounds, not the whole system.

The model includes all of these:

- One object, one controller process, and one reconcile at a time. The reconcile can stop between any two steps (restart or error).
- The informer cache lags at any point. The object read is cached, the Secret reads are live, and the status writes carry the resourceVersion.
- Coder commits or drops creates late. Secret writes land after the client gave up on them, and they fail when the Secret changed.
- A person deletes the object, edits the spec, overwrites or deletes the Secret, or deletes a key in Coder.
- A clock in 1-minute ticks. The settle window, the write age, and the room limits come from the Go source.

Bounds:

| Config | Creates (`MaxSerial`) | Clock (`MaxTime`) | Person actions | Distinct states | Time on this host |
| --- | --- | --- | --- | --- | --- |
| `quick` (default) | 2 | 8 min | 1 | 18,595,034 | 50 s, 16 workers |
| `deep` | 3 | 8 min | 1 | 209,344,657 | 9 min, 16 workers |

`StateBound` also caps the Secret's resourceVersion at 6 and the object's at 14.

Assumptions (constants that check.sh does not read from code):

- `ReqMax = 1`: the API server ends a request within 60 s. The docs state this assumption.
- `CommitMax = 2`: Coder commits or drops a create within 2 minutes after it gets it.
- `NoStall = TRUE`: the process does not pause between a time check or stamp and the request after it. The docs do not state this assumption. See [Known finding](#known-finding).
- Nobody but the operator writes a key of this object into the Secret.

Left out of the model:

- The `ControlPlaneGone` and bounded-pass releases, and the cleanup records and their sweep (`cleanup_record.go`, `cleanup_sweep.go`). A release there is a documented leak.
- `CoderDeploymentChanged`, stalls that come before the LIST (`SecretNotOwned`, `SecretNameReserved`, `NotAllowed`, `LicenseRequired`), the failure cap, and `CoderAnswerMismatch`.
- Grace periods, heartbeats, and the drift re-issue time. Retire steps, the drift stall, and scheduled rotations can happen at any time, which gives the model more behaviors, not fewer.
- Clock skew between leaders, and two leaders at once.
- The rule "no reconcile hits the `no room to retire` assertion". That rule needs 4 or more creates, and TLC found about 400 M states without finishing in 25 minutes. It is not verified.

## Does my change touch it?

Run `git diff --stat <sync commit> -- 'internal/controller/coderaigatewaykey_*.go' internal/controller/owned_secret.go`. Without `..HEAD`, the diff also shows uncommitted edits. The sync commit is [below](#sync). Each covered function maps to model actions:

| Go function | File | Model action |
| --- | --- | --- |
| `Reconcile`, `patchFinalizer` | `coderaigatewaykey_controller.go` | `ReconcileStarts`, `AddsFinalizer`, `ReleasesFinalizer` |
| `observe`, `aiGatewayKeySecretHolds`, `ownAIGatewayKeys`, `aiGatewayKeySerial` | `coderaigatewaykey_controller.go` | `ReadsSecret`, `ListsKeys`, `Decide` |
| `writeStatus` | `coderaigatewaykey_controller.go` | `WritesStatus`, `WritesIssueStatus` |
| `nextAIGatewayKeyAction`, `aiGatewayKeyOrphan`, `aiGatewayKeyDriftBlocked`, `aiGatewayKeyRearmed`, `aiGatewayKeyRotationDue`, `aiGatewayKeyRetireState` | `coderaigatewaykey_recover.go`, `_rotate.go`, `_retire.go` | `Decisions` (inside `Decide`) |
| `adoptAIGatewayKey`, `retireAIGatewayKey`, `setAIGatewayKeyCurrent` | `coderaigatewaykey_recover.go`, `_issue.go` | the `adopt` branch of `Decide`, `RecordsNewKey` |
| `sweepOrphan`, `retireKey`, `deleteOnly`, `deleteOwnKey`, `liveSecretHolds`, `dropAIGatewayKeyRetiring` | `coderaigatewaykey_recover.go`, `_retire.go` | `DeleteReadsSecret`, `DeletesKey` |
| `issueKey`, `aiGatewayKeyCreateFailure` | `coderaigatewaykey_issue.go` | `WritesMarker`, `SendsCreate`, `CreateSucceeds`, `CreateFails` |
| `writeKeySecret`, `writeOwnedSecret`, `getOwnedSecret` | `coderaigatewaykey_issue.go`, `owned_secret.go` | `WriteLoopReadsSecret`, `SendsSecretWrite`, `SecretWriteAnswered`, `SecretWriteTimesOut`, `SecretWriteLands` |
| `deleteStep`, `deleteKeys` | `coderaigatewaykey_delete.go` | `DeleteWaitsForSettle`, `DeleteLists`, `DeleteDeletes` |

`hack/formal/aigatewaykey/covered.sha256` holds a fingerprint of each function body. The check reports `STALE` when a body changed. A comment above a function is not part of its body.

## Run

```bash
hack/formal/aigatewaykey/check.sh                 # quick bound: about 1 minute with TLC_WORKERS=16, 2 minutes with the default 8
hack/formal/aigatewaykey/check.sh --config deep   # before you merge a change to the covered code
```

- Requirements: Java 11 or newer (`JAVA=/path/to/java` picks one), `curl`, and `sha256sum`. The first run downloads `tla2tools.jar` v1.7.4 from the TLA+ GitHub release, checks its SHA-256, and caches it in `${TLA2TOOLS_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/coder-k8s-tla}`.
- Knobs: `TLC_WORKERS` (default: the CPU count, at most 8), `TLC_HEAP` (default `8g`; give `deep` `24g`), `--out DIR` (default: a new temp folder), and `--set NAME=VALUE` to override one constant. The script prints each override, because an override replaces a value read from the code.
- The script first prints every fact it read from the Go source, with its file and line. A guard reads `TRUE` when its exact statement is in place, in the right order. The timing numbers are read from the constants.

Exit codes:

| Code | Meaning | What you do |
| --- | --- | --- |
| 0 | `PASS`, and no covered function changed | Done. |
| 1 | `VIOLATION` of a rule | Read the trace (next section). |
| 2 | The check could not run: an unreadable fact, a missing function, or a TLC error | Fix `check.sh` or the model to match the code. Do not skip the fact. |
| 3 | The rules hold, but covered code is `STALE` | Review the model against the changed functions (see [Update](#update-the-model-for-an-intended-change)). A `STALE` result is not a pass. |

## Read the result

Read every result with the pstack `/statechart` skill. Give it the `AIGatewayKey.tla` and `AIGatewayKey.cfg` that check.sh wrote into its `--out` folder, and the view `hack/formal/aigatewaykey/view.mjs`. The view labels each state with the object, the Secret, the keys in Coder, and the requests in flight. It labels each step in operator words. For a violation, `/statechart` gives the shortest path to each broken rule. The pass model has millions of states, which is too many for a chart, so read a passing run from check.sh output alone. `/statechart` checks with one worker, so a passing run takes much longer there.

A violation is a real bug or a modeling error. Tell them apart in this order:

1. Read the facts block. A guard that reads `FALSE` names the Go line that is gone or out of order. When your change removed or moved that line, the violation is the effect of your change. Restore the line, or prove the rule another way and update the model.
2. If every fact reads `TRUE`, replay the path against the code, one step per action, using the table above. A step that the code cannot take (for example, an answer before the request) is a modeling error. Fix the model and keep the rule.
3. A path that the code can take is a bug. Report it as the numbered path in code terms. Do not weaken a rule, an assumption, or a bound to make it pass.

## Update the model for an intended change

1. Change the model action that maps to the changed function. If the change adds a guard that a rule depends on, add a fact: a `has` or `before` line in check.sh, a `Guard*` constant in the model, and the guard in the action.
2. Break the new or changed fact once in a scratch edit. The check must fail with exit 1, or it must pass with `STALE` because another guard backs it up. Note which result you got, then drop the scratch edit.
3. Run `check.sh --config deep`. It must exit 0 or 3.
4. Run `check.sh --config deep --update-fingerprints`. The script writes `covered.sha256` only after TLC passes, and it refuses `--set` overrides. Then move the sync commit below to the commit that holds the reviewed code. Move it only after you have confirmed that the model describes the code at that commit.

## What the check catches

Each line is a scratch break of one fact in a copy of the sources, with the result of `check.sh` (2026-10-29):

| Scratch break | Result |
| --- | --- |
| `deleteOwnKey` deletes the key that the Secret holds (`if held == k.ID {` gone) | exit 1, `SecretKeyAlive` |
| `deleteOwnKey` no longer calls `liveSecretHolds` before the check | exit 1, `SecretKeyAlive` |
| `liveSecretHolds` reads the Secret from the cache (`r.Get`) | exit 1, `SecretKeyAlive` |
| `aiGatewayKeyOrphan` no longer skips serials at or above `NextSerial` | exit 1, `SecretKeyAlive` |
| `issueKey` skips the marker status write before the create | exit 1, `NoLeakAfterRelease` |
| `writeKeySecret` drops the write-age check | exit 1, `SecretKeyAlive` |
| `writeStatus` uses a merge patch in place of `Status().Update` | exit 1, `NoLeakAfterRelease` |
| `writeOwnedSecret` clears the resourceVersion | exit 1, `SecretKeyAlive` |
| `deleteStep` skips the settle wait | exit 1, `NoLeakAfterRelease` |
| `Reconcile` skips the finalizer before observe | exit 1, `NoLeakAfterRelease` |
| Settle window 2 minutes | exit 1, `NoLeakAfterRelease` |
| Write age 5 minutes | exit 1, `SecretKeyAlive` |
| Write age 4 minutes | exit 0. Correct: a write that starts before minute 4 lands before minute 5. The model does not model the clock-skew margin. |
| `aiGatewayKeyOrphan` no longer skips the held key | exit 3 (`STALE`). The live read in `deleteOwnKey` backs it up. |
| `aiGatewayKeyRetireState` no longer skips the held key | exit 3 (`STALE`). The live read backs it up, and the model has no writer that puts a retiring key back into the Secret. |
| A message change inside `deleteOwnKey` | exit 3 (`STALE`) |
| A comment change above `deleteOwnKey` | exit 0 |

Facts check that a line is present and in order. They do not check what the line means. So treat `STALE` as a reason to review, never as a pass.

## Known finding

`check.sh --config stall` sets `NoStall = FALSE` and fails `NoLeakAfterRelease` in 19 steps:

1. A reconcile writes the create marker, which sets `lastCreateAttemptTime` to T (`issueKey`, `coderaigatewaykey_issue.go`).
2. The process pauses for 5 minutes before it sends `CreateAIGatewayKey`.
3. A person deletes the object. The operator sends the create, then the reconcile stops.
4. A new reconcile takes the delete path. T plus 5 minutes has passed, so `deleteStep` lists the keys, finds none, and removes the finalizer.
5. Coder commits the create. The key has no owner.

`writeKeySecret` refuses to start a Secret write later than `aiGatewayKeyMaxWriteAge` after T. Nothing bounds the time from T to the create request in the same way. A real case: a leader that freezes (a long GC pause or a frozen VM), loses its lease, and then resumes. The docs list only the clock-skew and request-timeout assumptions. The finding is not fixed and not replayed live. A check of `r.Clock.Since(attemptStart)` right before the create would make the remaining gap as small as the gap that the write age leaves.

## Sync

The model describes the covered code at commit `c9dab9ec2e2615fbd1cfbbbfa4c1a6512d526960` (2026-10-15). The last change to a covered file before it is `aab11e7b84766d6fbdb4c57c2a34b1cf6b7c0f8f`. `covered.sha256` holds the fingerprints at that commit. To bring the model back in line after changes that skipped this check, use the pstack `/maintain-formal-verification-skill` skill.
