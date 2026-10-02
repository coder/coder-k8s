package storage

import "time"

// MaxWorkspaceLogDuration bounds one coderworkspaces/log request: after it, the request reads
// nothing more from Coder. It stays below the aggregated API request timeout; the server checks
// that at startup.
const MaxWorkspaceLogDuration = 25 * time.Minute

// MaxWorkspaceLogSnapshotDuration bounds a coderworkspaces/log snapshot request. In Kind, a
// snapshot of a 1 MB-capped build (1.66 MB rendered) took 0.39 s alone and 1.4 s at p99 with 64
// concurrent requests, so 60 s leaves a wide margin while a stalled Coder holds a slot for at most
// one minute.
const MaxWorkspaceLogSnapshotDuration = 60 * time.Second
