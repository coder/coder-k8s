---------------------------- MODULE AIGatewayKey ----------------------------
(***************************************************************************)
(* A model of one CoderAIGatewayKey object and the operator that keeps its *)
(* AI Gateway key (internal/controller/coderaigatewaykey_*.go).            *)
(*                                                                         *)
(* A key is its serial: the operator names keys ck8s-<UID>-<serial>, and   *)
(* Coder refuses a second key with the same name. One controller process   *)
(* runs one reconcile at a time (controller-runtime serializes a key). It  *)
(* reads the object from the informer cache, which can lag; it reads the   *)
(* output Secret live. A reconcile can stop between any two steps (crash,  *)
(* error return). Requests that the controller gave up on can still land.  *)
(* One clock tick is one minute.                                           *)
(*                                                                         *)
(* check.sh reads every Guard* constant and the timing constants from the  *)
(* Go source. Run the check with check.sh, not with TLC directly.          *)
(***************************************************************************)
EXTENDS Integers, FiniteSets

CONSTANTS
    MaxSerial,      \* Model bound: creates use serials 0..MaxSerial-1.
    MaxTime,        \* Model bound: the clock stops here.
    MaxUserActs,    \* Model bound: number of actions by a person.
    Settle,         \* aiGatewayKeySettleWindow, in minutes.
    WriteAge,       \* aiGatewayKeyMaxWriteAge, in minutes.
    MaxRetiring,    \* aiGatewayKeyMaxRetiring.
    SecretTries,    \* aiGatewayKeySecretWriteTries.
    RotateScheduledRoom, \* aiGatewayKeyRotationDue: a scheduled rotation needs fewer retiring keys than this.
    RotateRequestedRoom, \* aiGatewayKeyRotationDue: a requested rotation needs fewer retiring keys than this.
    CommitMax,      \* Assumption: Coder commits or drops a create within this many minutes after it gets it.
    ReqMax,         \* Assumption: the API server ends a request within this many minutes (60 s).
    NoStall,        \* Assumption: the process does not pause between a time check and the request after it.
    GuardLiveReadBeforeDelete, \* deleteOwnKey: no DELETE of the key that the live Secret holds.
    GuardOrphanSkipsHeld,      \* aiGatewayKeyOrphan: the held key is never an orphan.
    GuardRetireSkipsHeld,      \* aiGatewayKeyRetireState: the held key is never due.
    GuardSerialGate,           \* Serials >= status.nextSerial are not this object's.
    GuardMarkerBeforeCreate,   \* issueKey: the marker status write runs before the create.
    GuardWriteAge,             \* writeKeySecret: no Secret write starts after aiGatewayKeyMaxWriteAge.
    GuardStatusCAS,            \* writeStatus: Status().Update with the read resourceVersion.
    GuardSecretCAS,            \* writeOwnedSecret: Update of the Secret that was read.
    GuardDeleteSettle,         \* deleteStep: the LIST waits for the settle window.
    GuardFinalizerFirst        \* Reconcile: the finalizer is stored before observe.

None == -1
Garbage == -2   \* A Secret value that holds no key of this object.
Serials == 0..(MaxSerial - 1)
Min(S) == CHOOSE x \in S : \A y \in S : x <= y

VARIABLES
    now,        \* The clock, in minutes.
    coder,      \* Serials of the keys of this object that Coder has.
    killed,     \* Serials that a person deleted in Coder (the kill switch).
    creates,    \* Create requests sent to Coder and not committed or dropped: [s, t].
    secret,     \* The value of the output Secret: a serial, None (no Secret) or Garbage.
    secretVer,  \* The Secret's resourceVersion.
    swrites,    \* Secret writes sent and not landed: [k, v, t, waited].
    wres,       \* Answer of the write that the controller waits for: "none", "ok", "conflict".
    st,         \* The stored status of the object.
    rv,         \* The object's resourceVersion.
    gen,        \* The object's generation (spec edits).
    deleting,   \* The object has a deletionTimestamp.
    fin,        \* The object has the finalizer coder.com/ai-gateway-key-cleanup.
    cache,      \* The informer cache's copy of the object.
    userActs,   \* Actions by a person so far.
    assertFailed, \* A reconcile hit an "assertion failed: ... retire" error.
    pc,         \* Where the reconcile is.
    view,       \* The object that this reconcile read (from the cache).
    hold,       \* The live Secret value that observe read.
    listed,     \* The keys that the LIST answered.
    act,        \* The decision of this reconcile.
    tgt,        \* The key that this reconcile deletes or issues.
    mySt,       \* The status that this reconcile writes.
    myRV,       \* The resourceVersion that the next status write sends.
    attT,       \* The marker time of this reconcile's create attempt.
    tries,      \* Secret write tries of this attempt.
    readVer,    \* Secret resourceVersion of the last live read in the write loop.
    dset        \* Keys that this delete pass still deletes.

vars == <<now, coder, killed, creates, secret, secretVer, swrites, wres, st, rv, gen,
          deleting, fin, cache, userActs, assertFailed, pc, view, hold, listed, act,
          tgt, mySt, myRV, attT, tries, readVer, dset>>
locals == <<view, hold, listed, act, tgt, mySt, myRV, attT, tries, readVer, dset>>

\* status fields: next = nextSerial, cur = currentKey, ret = retiringKeys,
\* pend = pendingKey, last = lastCreateAttemptTime, applied = the generation
\* whose trigger the current key handled, kg = killSwitchGeneration.
InitStatus == [next |-> 0, cur |-> None, ret |-> {}, pend |-> None, last |-> None, applied |-> 0, kg |-> 0]
Snapshot == [st |-> st, rv |-> rv, gen |-> gen, del |-> deleting, fin |-> fin]
Gone == deleting /\ ~fin
CASok(r) == ~GuardStatusCAS \/ r = rv

ResetLocals ==
    /\ view' = [st |-> InitStatus, rv |-> 0, gen |-> 0, del |-> FALSE, fin |-> FALSE]
    /\ hold' = None /\ listed' = {} /\ act' = "" /\ tgt' = None /\ mySt' = InitStatus
    /\ myRV' = 0 /\ attT' = 0 /\ tries' = 0 /\ readVer' = 0 /\ dset' = {}

Init ==
    /\ now = 0 /\ coder = {} /\ killed = {} /\ creates = {}
    /\ secret = None /\ secretVer = 0 /\ swrites = {} /\ wres = "none"
    /\ st = InitStatus /\ rv = 1 /\ gen = 1 /\ deleting = FALSE /\ fin = FALSE
    /\ cache = [st |-> InitStatus, rv |-> 1, gen |-> 1, del |-> FALSE, fin |-> FALSE]
    /\ userActs = 0 /\ assertFailed = FALSE /\ pc = "idle"
    /\ view = [st |-> InitStatus, rv |-> 0, gen |-> 0, del |-> FALSE, fin |-> FALSE]
    /\ hold = None /\ listed = {} /\ act = "" /\ tgt = None /\ mySt = InitStatus
    /\ myRV = 0 /\ attT = 0 /\ tries = 0 /\ readVer = 0 /\ dset = {}

-----------------------------------------------------------------------------
(* The world: time, Coder, the API server, the cache, and a person.        *)

\* Time passes only while every request can still meet its deadline: these
\* are the timing assumptions of docs/how-to/manage-ai-gateway-keys.md.
Tick ==
    /\ now < MaxTime
    /\ \A c \in creates : now + 1 <= c.t + CommitMax
    /\ \A w \in swrites : now + 1 <= w.t + ReqMax
    \* The cleanup record write and the marker write run between the time stamp
    \* and the create, each within ReqMax. A Secret write starts right after
    \* its age check.
    /\ NoStall => (/\ (pc \in {"create", "marker2"} => now + 1 <= attT + 2 * ReqMax)
                   /\ pc /= "wsend")
    /\ now' = now + 1
    /\ UNCHANGED <<coder, killed, creates, secret, secretVer, swrites, wres, st, rv, gen,
                   deleting, fin, cache, userActs, assertFailed, pc>>
    /\ UNCHANGED locals

\* Coder commits a create. Names are unique, so a duplicate commits nothing.
CoderCommitsCreate ==
    \E c \in creates :
        /\ creates' = creates \ {c}
        /\ coder' = IF c.s \in coder \/ c.s \in killed THEN coder ELSE coder \cup {c.s}
        /\ UNCHANGED <<now, killed, secret, secretVer, swrites, wres, st, rv, gen,
                       deleting, fin, cache, userActs, assertFailed, pc>>
        /\ UNCHANGED locals

CoderDropsCreate ==
    \E c \in creates :
        /\ creates' = creates \ {c}
        /\ UNCHANGED <<now, coder, killed, secret, secretVer, swrites, wres, st, rv, gen,
                       deleting, fin, cache, userActs, assertFailed, pc>>
        /\ UNCHANGED locals

\* A Secret write lands at the API server. With resourceVersion it fails when
\* the Secret changed after the read.
SecretWriteLands ==
    \E w \in swrites :
        LET ok == ~GuardSecretCAS \/ w.v = secretVer IN
        /\ swrites' = swrites \ {w}
        /\ secret' = IF ok THEN w.k ELSE secret
        /\ secretVer' = IF ok THEN secretVer + 1 ELSE secretVer
        /\ wres' = IF w.waited THEN (IF ok THEN "ok" ELSE "conflict") ELSE wres
        /\ UNCHANGED <<now, coder, killed, creates, st, rv, gen,
                       deleting, fin, cache, userActs, assertFailed, pc>>
        /\ UNCHANGED locals

\* The informer cache catches up. It can stop at any stored version, because
\* this action can run after any write.
CacheSyncs ==
    /\ cache /= Snapshot
    /\ cache' = Snapshot
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres, st, rv, gen,
                   deleting, fin, userActs, assertFailed, pc>>
    /\ UNCHANGED locals

PersonCanAct == userActs < MaxUserActs /\ userActs' = userActs + 1

\* A person deletes the object. Without the finalizer it is gone at once.
PersonDeletesObject ==
    /\ PersonCanAct
    /\ ~deleting
    /\ deleting' = TRUE /\ rv' = rv + 1
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres, st, gen,
                   fin, cache, assertFailed, pc>>
    /\ UNCHANGED locals

\* A person edits the spec, for example spec.rotation.trigger.
PersonEditsSpec ==
    /\ PersonCanAct
    /\ ~deleting
    /\ gen' = gen + 1 /\ rv' = rv + 1
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres, st,
                   deleting, fin, cache, assertFailed, pc>>
    /\ UNCHANGED locals

\* A person or another writer overwrites or deletes the Secret. Nobody but
\* the operator writes a key of this object into it (an assumption).
PersonBreaksSecret ==
    /\ PersonCanAct
    /\ \E v \in {None, Garbage} : secret' = v
    /\ secretVer' = secretVer + 1
    /\ UNCHANGED <<now, coder, killed, creates, swrites, wres, st, rv, gen,
                   deleting, fin, cache, assertFailed, pc>>
    /\ UNCHANGED locals

\* A person deletes a key in Coder (coder ai-gateway keys delete).
PersonDeletesKeyInCoder ==
    /\ PersonCanAct
    /\ \E s \in coder : coder' = coder \ {s} /\ killed' = killed \cup {s}
    /\ UNCHANGED <<now, creates, secret, secretVer, swrites, wres, st, rv, gen,
                   deleting, fin, cache, assertFailed, pc>>
    /\ UNCHANGED locals

-----------------------------------------------------------------------------
(* The controller. Each action is one step of Reconcile.                   *)

Ctl(p, q) == pc = p /\ pc' = q
KeepWorld == UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres,
                         st, rv, gen, deleting, fin, cache, userActs>>

\* The process restarts, or the reconcile returns an error. Memory is lost;
\* requests in flight stay in flight, with nobody waiting for them.
ReconcileStops ==
    /\ pc /= "idle"
    /\ pc' = "idle"
    /\ swrites' = {[w EXCEPT !.waited = FALSE] : w \in swrites}
    /\ wres' = "none"
    /\ ResetLocals
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, st, rv, gen,
                   deleting, fin, cache, userActs, assertFailed>>

\* Reconcile: r.Get from the cache, then the deletion branch or the
\* finalizer patch, then observe.
ReconcileStarts ==
    /\ pc = "idle"
    /\ cache.rv > 0
    /\ ~(cache.del /\ ~cache.fin)        \* NotFound or nothing to do.
    /\ ~Gone
    /\ view' = cache
    /\ pc' = IF cache.del THEN "dwait"
             ELSE IF GuardFinalizerFirst /\ ~cache.fin THEN "addfin"
             ELSE "sread"
    /\ UNCHANGED <<hold, listed, act, tgt, mySt, myRV, attT, tries, readVer, dset>>
    /\ KeepWorld /\ UNCHANGED assertFailed

\* patchFinalizer(add) with an optimistic lock.
AddsFinalizer ==
    /\ pc = "addfin"
    /\ IF view.rv = rv
       THEN /\ fin' = TRUE /\ rv' = rv + 1
            /\ view' = [view EXCEPT !.rv = rv + 1, !.fin = TRUE]
            /\ pc' = "sread"
            /\ UNCHANGED <<hold, listed, act, tgt, mySt, myRV, attT, tries, readVer, dset>>
       ELSE /\ pc' = "idle" /\ ResetLocals /\ UNCHANGED <<fin, rv>>
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres,
                   st, gen, deleting, cache, userActs, assertFailed>>

\* observe: getOwnedSecret (live), then ListAIGatewayKeys.
ReadsSecret ==
    /\ Ctl("sread", "list")
    /\ hold' = secret
    /\ UNCHANGED <<view, listed, act, tgt, mySt, myRV, attT, tries, readVer, dset>>
    /\ KeepWorld /\ UNCHANGED assertFailed

ListsKeys ==
    /\ Ctl("list", "decide")
    /\ listed' = coder
    /\ UNCHANGED <<view, hold, act, tgt, mySt, myRV, attT, tries, readVer, dset>>
    /\ KeepWorld /\ UNCHANGED assertFailed

\* nextAIGatewayKeyAction and the observe step around it. Returns the set of
\* decisions that the code can take: grace periods, heartbeats and the
\* drift time are not modeled, so retire steps, the drift stall and the
\* scheduled rotation can happen at any time.
Decisions(v, L, h) ==
    LET s0a == v.st
        curListed == s0a.cur /= None /\ s0a.cur \in L
        \* dropAIGatewayKeyRetiring and KillSwitchGeneration = 0 when Coder lists the current key.
        s0 == IF curListed THEN [s0a EXCEPT !.ret = s0a.ret \cap L, !.kg = 0] ELSE s0a
        created(k) == ~GuardSerialGate \/ k < s0.next
        heldK == IF h \in L THEN h ELSE None
        orphans == {k \in L : created(k) /\ k /= s0.cur /\ k \notin s0.ret
                              /\ (GuardOrphanSkipsHeld => k /= h)}
        hasOrphan == orphans /= {}
        orphan == IF hasOrphan THEN Min(orphans) ELSE None
        settleStart == IF s0.last = None THEN now ELSE s0.last
        settled == hasOrphan /\ now >= settleStart + Settle
        healthy == s0.cur /= None /\ heldK = s0.cur
        due == {k \in s0.ret \cap L : (GuardRetireSkipsHeld => k /= h) /\ k /= s0.cur}
        D(kind, k, s) == [kind |-> kind, k |-> k, st |-> s]
        \* deleteOnly: the retire step or the sweep, never a create.
        cleanup(s) == {D("retire", k, s) : k \in due}
                      \cup (IF settled THEN {D("sweep", orphan, s)} ELSE {D("wait", None, s)})
        canCreate == s0.next < MaxSerial   \* Model bound, not a code rule.
    IN
    IF heldK /= None /\ heldK /= s0.cur /\ created(heldK) /\ (s0.cur = None \/ heldK > s0.cur)
    THEN {D("adopt", heldK, s0)}
    ELSE IF s0.pend /= None THEN {D("recover", None, s0)}
    ELSE IF s0.cur = None
    THEN IF settled THEN {D("sweep", orphan, s0)}
         ELSE IF hasOrphan THEN {D("wait", None, s0)}
         ELSE IF canCreate THEN {D("issue", s0.next, s0)} ELSE {D("wait", None, s0)}
    ELSE IF s0.cur \notin L
    THEN IF s0.kg > 0 /\ s0.kg < v.gen
         THEN {D("rearm", None, [s0 EXCEPT !.cur = None, !.kg = 0])}
         ELSE cleanup([s0 EXCEPT !.kg = IF s0.kg = 0 THEN v.gen ELSE s0.kg])
    ELSE IF ~healthy
    THEN \* aiGatewayKeyDriftBlocked: a full retiring list, or a re-issue inside the grace period.
         cleanup(s0)
         \cup (IF Cardinality(s0.ret) < MaxRetiring
               THEN (IF settled THEN {D("sweep", orphan, s0)}
                     ELSE IF hasOrphan THEN {D("wait", None, s0)}
                     ELSE IF canCreate THEN {D("reissue", s0.next, s0)} ELSE {})
               ELSE {})
    ELSE {D("retire", k, s0) : k \in due}
         \cup (IF settled THEN {D("sweep", orphan, s0)}
               ELSE IF hasOrphan THEN {D("wait", None, s0)}
               ELSE \* A requested rotation (trigger) and a scheduled one each wait for room.
                    (IF canCreate /\ ((v.gen > s0.applied /\ Cardinality(s0.ret) < RotateRequestedRoom)
                                       \/ Cardinality(s0.ret) < RotateScheduledRoom)
                     THEN {D("rotate", s0.next, s0)} ELSE {})
                    \cup {D("wait", None, s0)})

Decide ==
    /\ pc = "decide"
    /\ \E d \in Decisions(view, listed, hold) :
        /\ act' = d.kind /\ tgt' = d.k /\ myRV' = view.rv
        /\ CASE d.kind = "adopt" ->
                  \* adoptAIGatewayKey: the previous current key retires when listed.
                  LET prev == d.st.cur
                      retires == prev /= None /\ prev \in listed
                  IN IF retires /\ Cardinality(d.st.ret) >= MaxRetiring
                     THEN /\ assertFailed' = TRUE /\ pc' = "idle" /\ mySt' = d.st
                     ELSE /\ mySt' = [d.st EXCEPT !.cur = d.k, !.kg = 0,
                                        !.ret = IF retires THEN d.st.ret \cup {prev} ELSE d.st.ret,
                                        !.pend = IF d.st.pend = d.k THEN None ELSE d.st.pend]
                          /\ pc' = "wstatus" /\ UNCHANGED assertFailed
             [] d.kind = "recover" ->
                  /\ mySt' = [d.st EXCEPT !.pend = None] /\ pc' = "wstatus" /\ UNCHANGED assertFailed
             [] d.kind \in {"sweep", "retire"} ->
                  /\ mySt' = d.st /\ pc' = "dlive" /\ UNCHANGED assertFailed
             [] d.kind \in {"issue", "reissue", "rotate"} ->
                  \* issueKey's first assertion: room to retire the current key.
                  IF d.st.cur /= None /\ Cardinality(d.st.ret) >= MaxRetiring
                  THEN /\ assertFailed' = TRUE /\ pc' = "idle" /\ mySt' = d.st
                  ELSE /\ mySt' = d.st /\ pc' = "marker" /\ UNCHANGED assertFailed
             [] OTHER ->
                  /\ mySt' = d.st /\ pc' = "wstatus" /\ UNCHANGED assertFailed
    /\ UNCHANGED <<view, hold, listed, attT, tries, readVer, dset>>
    /\ KeepWorld

\* deleteOwnKey: the I1 live read of the Secret, then the DELETE.
DeleteReadsSecret ==
    /\ pc = "dlive"
    /\ pc' = IF GuardLiveReadBeforeDelete /\ secret = tgt THEN "wstatus" ELSE "ddelete"
    /\ UNCHANGED locals
    /\ KeepWorld /\ UNCHANGED assertFailed

DeletesKey ==
    /\ Ctl("ddelete", "wstatus")
    /\ coder' = coder \ {tgt}
    \* retireKey drops the key from status.retiringKeys after a 204 or 404.
    /\ mySt' = IF act = "retire" THEN [mySt EXCEPT !.ret = mySt.ret \ {tgt}] ELSE mySt
    /\ UNCHANGED <<view, hold, listed, act, tgt, myRV, attT, tries, readVer, dset>>
    /\ UNCHANGED <<now, killed, creates, secret, secretVer, swrites, wres,
                   st, rv, gen, deleting, fin, cache, userActs, assertFailed>>

\* writeStatus at the end of Reconcile: no write when nothing changed.
WritesStatus ==
    /\ pc = "wstatus"
    /\ pc' = "idle"
    /\ IF mySt /= view.st /\ CASok(myRV)
       THEN st' = mySt /\ rv' = rv + 1
       ELSE UNCHANGED <<st, rv>>
    /\ ResetLocals
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres,
                   gen, deleting, fin, cache, userActs, assertFailed>>

\* issueKey: the marker (pendingKey, nextSerial + 1, lastCreateAttemptTime)
\* in one optimistic status update, before the create.
WritesMarker ==
    /\ pc = "marker"
    /\ attT' = now
    /\ LET m == [mySt EXCEPT !.next = tgt + 1, !.pend = tgt, !.last = now] IN
       IF ~GuardMarkerBeforeCreate
       THEN /\ mySt' = m /\ pc' = "create" /\ UNCHANGED <<st, rv, myRV>>
       ELSE IF CASok(myRV)
       THEN /\ st' = m /\ rv' = rv + 1 /\ myRV' = rv + 1 /\ mySt' = m /\ pc' = "create"
       ELSE /\ pc' = "idle" /\ UNCHANGED <<st, rv, myRV, mySt>>
    /\ UNCHANGED <<view, hold, listed, act, tgt, tries, readVer, dset>>
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres,
                   gen, deleting, fin, cache, userActs, assertFailed>>

SendsCreate ==
    /\ pc = "create"
    /\ creates' = creates \cup {[s |-> tgt, t |-> now]}
    /\ pc' = IF GuardMarkerBeforeCreate THEN "await" ELSE "marker2"
    /\ UNCHANGED locals
    /\ UNCHANGED <<now, coder, killed, secret, secretVer, swrites, wres,
                   st, rv, gen, deleting, fin, cache, userActs, assertFailed>>

\* Only a variant without GuardMarkerBeforeCreate runs this: the marker after the create.
WritesMarkerLate ==
    /\ pc = "marker2"
    /\ IF CASok(myRV)
       THEN /\ st' = mySt /\ rv' = rv + 1 /\ myRV' = rv + 1 /\ pc' = "await"
       ELSE /\ pc' = "idle" /\ UNCHANGED <<st, rv, myRV>>
    /\ UNCHANGED <<view, hold, listed, act, tgt, mySt, attT, tries, readVer, dset>>
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres,
                   gen, deleting, fin, cache, userActs, assertFailed>>

\* Coder answers 201: only after the commit.
CreateSucceeds ==
    /\ pc = "await"
    /\ tgt \in coder
    /\ \A c \in creates : c.s /= tgt
    /\ pc' = "wread" /\ tries' = 0
    /\ UNCHANGED <<view, hold, listed, act, tgt, mySt, myRV, attT, readVer, dset>>
    /\ KeepWorld /\ UNCHANGED assertFailed

\* An uncertain or failed answer: pendingKey goes, the serial stays used.
CreateFails ==
    /\ Ctl("await", "wstatus2")
    /\ mySt' = [mySt EXCEPT !.pend = None]
    /\ UNCHANGED <<view, hold, listed, act, tgt, myRV, attT, tries, readVer, dset>>
    /\ KeepWorld /\ UNCHANGED assertFailed

\* writeKeySecret: a live read before each try.
WriteLoopReadsSecret ==
    /\ pc = "wread"
    /\ IF secret = tgt THEN pc' = "final" /\ UNCHANGED <<mySt, readVer>>            \* An earlier try landed.
       ELSE IF tries = SecretTries \/ (GuardWriteAge /\ now >= attT + WriteAge)
       THEN pc' = "wstatus2" /\ mySt' = [mySt EXCEPT !.pend = None] /\ UNCHANGED readVer
       ELSE pc' = "wsend" /\ readVer' = secretVer /\ UNCHANGED mySt
    /\ UNCHANGED <<view, hold, listed, act, tgt, myRV, attT, tries, dset>>
    /\ KeepWorld /\ UNCHANGED assertFailed

SendsSecretWrite ==
    /\ pc = "wsend"
    /\ pc' = "wwait"
    /\ swrites' = swrites \cup {[k |-> tgt, v |-> readVer, t |-> now, waited |-> TRUE]}
    /\ wres' = "none"
    /\ UNCHANGED locals
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, st, rv, gen,
                   deleting, fin, cache, userActs, assertFailed>>

SecretWriteAnswered ==
    /\ pc = "wwait"
    /\ wres /= "none"
    /\ pc' = IF wres = "ok" THEN "final" ELSE "wread"
    /\ tries' = IF wres = "ok" THEN tries ELSE tries + 1
    /\ wres' = "none"
    /\ UNCHANGED <<view, hold, listed, act, tgt, mySt, myRV, attT, readVer, dset>>
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, st, rv, gen,
                   deleting, fin, cache, userActs, assertFailed>>

\* The client gives up on the write; the request can still land.
SecretWriteTimesOut ==
    /\ pc = "wwait"
    /\ wres = "none"
    /\ pc' = "wread" /\ tries' = tries + 1
    /\ swrites' = {[w EXCEPT !.waited = FALSE] : w \in swrites}
    /\ UNCHANGED <<view, hold, listed, act, tgt, mySt, myRV, attT, readVer, dset>>
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, wres, st, rv, gen,
                   deleting, fin, cache, userActs, assertFailed>>

\* The Secret holds the new key: it becomes current, the old one retires.
RecordsNewKey ==
    /\ pc = "final"
    /\ LET prev == mySt.cur IN
       IF prev /= None /\ Cardinality(mySt.ret) >= MaxRetiring
       THEN /\ assertFailed' = TRUE /\ pc' = "idle" /\ UNCHANGED mySt
       ELSE /\ mySt' = [mySt EXCEPT !.pend = None, !.cur = tgt, !.applied = view.gen, !.kg = 0,
                          !.ret = IF prev /= None THEN mySt.ret \cup {prev} ELSE mySt.ret]
            /\ pc' = "wstatus2" /\ UNCHANGED assertFailed
    /\ UNCHANGED <<view, hold, listed, act, tgt, myRV, attT, tries, readVer, dset>>
    /\ KeepWorld

\* The status write of the issue step, with the marker's resourceVersion.
WritesIssueStatus ==
    /\ pc = "wstatus2"
    /\ pc' = "idle"
    /\ IF CASok(myRV) THEN st' = mySt /\ rv' = rv + 1 ELSE UNCHANGED <<st, rv>>
    /\ ResetLocals
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres,
                   gen, deleting, fin, cache, userActs, assertFailed>>

\* deleteStep: no create was ever sent, or the settle window has passed.
DeleteWaitsForSettle ==
    /\ pc = "dwait"
    /\ pc' = IF view.st.next = 0 THEN "release"
             ELSE IF GuardDeleteSettle /\ (view.st.last = None \/ now < view.st.last + Settle) THEN "idle"
             ELSE "dlist"
    /\ IF pc' = "idle" THEN ResetLocals ELSE UNCHANGED locals
    /\ KeepWorld /\ UNCHANGED assertFailed

\* deleteKeys: LIST, keep serials below nextSerial.
DeleteLists ==
    /\ pc = "dlist"
    /\ LET own == {k \in coder : k < view.st.next} IN
       /\ dset' = own
       /\ pc' = IF own = {} THEN "release" ELSE "ddel"
    /\ UNCHANGED <<view, hold, listed, act, tgt, mySt, myRV, attT, tries, readVer>>
    /\ KeepWorld /\ UNCHANGED assertFailed

DeleteDeletes ==
    /\ pc = "ddel"
    /\ \E k \in dset :
        /\ coder' = coder \ {k}
        /\ dset' = dset \ {k}
        /\ pc' = IF dset \ {k} = {} THEN "dlist" ELSE "ddel"
    /\ UNCHANGED <<view, hold, listed, act, tgt, mySt, myRV, attT, tries, readVer>>
    /\ UNCHANGED <<now, killed, creates, secret, secretVer, swrites, wres,
                   st, rv, gen, deleting, fin, cache, userActs, assertFailed>>

\* patchFinalizer(remove) with an optimistic lock.
ReleasesFinalizer ==
    /\ pc = "release"
    /\ pc' = "idle"
    /\ IF view.rv = rv THEN fin' = FALSE /\ rv' = rv + 1 ELSE UNCHANGED <<fin, rv>>
    /\ ResetLocals
    /\ UNCHANGED <<now, coder, killed, creates, secret, secretVer, swrites, wres,
                   st, gen, deleting, cache, userActs, assertFailed>>

Next ==
    \/ Tick \/ CoderCommitsCreate \/ CoderDropsCreate \/ SecretWriteLands \/ CacheSyncs
    \/ PersonDeletesObject \/ PersonEditsSpec \/ PersonBreaksSecret \/ PersonDeletesKeyInCoder
    \/ ReconcileStops \/ ReconcileStarts \/ AddsFinalizer \/ ReadsSecret \/ ListsKeys \/ Decide
    \/ DeleteReadsSecret \/ DeletesKey \/ WritesStatus
    \/ WritesMarker \/ SendsCreate \/ WritesMarkerLate \/ CreateSucceeds \/ CreateFails
    \/ WriteLoopReadsSecret \/ SendsSecretWrite \/ SecretWriteAnswered \/ SecretWriteTimesOut
    \/ RecordsNewKey \/ WritesIssueStatus
    \/ DeleteWaitsForSettle \/ DeleteLists \/ DeleteDeletes \/ ReleasesFinalizer

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* The rules.                                                              *)

\* While the object exists, the Secret never holds a key that Coder lost,
\* unless a person deleted that key in Coder.
SecretKeyAlive == (~deleting /\ secret \in Serials) => (secret \in coder \/ secret \in killed)

\* When the finalizer goes, Coder has no key of the object and no create of
\* the object can still commit.
NoLeakAfterRelease == Gone => (coder = {} /\ creates = {})

\* Bound the model: TLC stops exploring past this.
StateBound == secretVer <= 6 /\ rv <= 14
=============================================================================
