// A /statechart view for AIGatewayKey.tla: labels in the words of an
// operator user. Pass it with --view when you read a check result.
const keys = (s) => (s.length === 0 ? "none" : [...s].sort().map((k) => `ck8s-…-${k}`).join(", "));
const value = (v) => (v === -1 ? "no Secret" : v === -2 ? "no key" : `ck8s-…-${v}`);

export const state = (v) => {
  const object = !v.deleting ? "object exists" : v.fin ? "object deleting" : "object gone";
  const creates = [...v.creates].map((c) => c.s);
  const writes = [...v.swrites].map((w) => w.k);
  const inFlight =
    (creates.length ? ` · create in flight: ${keys(creates)}` : "") +
    (writes.length ? ` · Secret write in flight: ${keys(writes)}` : "");
  return `${object} · Secret: ${value(v.secret)} · Coder: ${keys(v.coder)}${inFlight}`;
};

const words = {
  Tick: "a minute passes",
  CoderCommitsCreate: "Coder commits a key create",
  CoderDropsCreate: "Coder drops a key create",
  SecretWriteLands: "a Secret write lands",
  CacheSyncs: "the informer cache catches up",
  PersonDeletesObject: "a person deletes the CoderAIGatewayKey",
  PersonEditsSpec: "a person edits the spec",
  PersonBreaksSecret: "a person overwrites or deletes the Secret",
  PersonDeletesKeyInCoder: "a person deletes a key in Coder",
  ReconcileStops: "the reconcile stops (restart or error)",
  ReconcileStarts: "a reconcile reads the object from the cache",
  AddsFinalizer: "the operator adds the finalizer",
  ReadsSecret: "the operator reads the Secret",
  ListsKeys: "the operator lists the keys in Coder",
  Decide: "the operator picks its action",
  DeleteReadsSecret: "the operator reads the Secret before a delete",
  DeletesKey: "the operator deletes a key in Coder",
  WritesStatus: "the operator writes status",
  WritesMarker: "the operator writes the create marker",
  SendsCreate: "the operator sends a key create",
  WritesMarkerLate: "the operator writes the create marker after the create",
  CreateSucceeds: "Coder answers the create with the key",
  CreateFails: "the create answer fails or is lost",
  WriteLoopReadsSecret: "the operator reads the Secret before a write",
  SendsSecretWrite: "the operator sends a Secret write",
  SecretWriteAnswered: "the Secret write answers",
  SecretWriteTimesOut: "the operator gives up waiting for a Secret write",
  RecordsNewKey: "the operator records the new key",
  WritesIssueStatus: "the operator writes the issue status",
  DeleteWaitsForSettle: "the delete checks the settle window",
  DeleteLists: "the delete lists the keys in Coder",
  DeleteDeletes: "the delete removes a key in Coder",
  ReleasesFinalizer: "the operator removes the finalizer",
};

export const step = (action) => words[action] ?? action;

export const title = "CoderAIGatewayKey key lifecycle";
