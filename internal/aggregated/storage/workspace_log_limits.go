package storage

import "time"

// MaxWorkspaceLogDuration bounds one coderworkspaces/log request: after it, the request reads
// nothing more from Coder. It stays below the aggregated API request timeout; the server checks
// that at startup.
const MaxWorkspaceLogDuration = 25 * time.Minute
