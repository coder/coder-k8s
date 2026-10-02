package storage

import (
	"context"
	"io"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// followLog streams the build's entries after afterID until Coder closes the stream (the build
// ended), remaining bytes are written, or ctx ends. ctx is the log deadline: when it ends, the
// Coder stream and the pipe are closed even if the client stopped reading, so nothing more is
// read from Coder. codersdk reads ignore ctx, which is why the stream is closed explicitly.
func (w *workspaceLogStream) followLog(ctx context.Context, sdk *codersdk.Client, buildID uuid.UUID, afterID, remaining int64) (io.Reader, error) {
	if remaining <= 0 {
		panic("assertion failed: follow needs a positive byte budget")
	}
	// codersdk drops the client's request timeout for this handshake, so bound it here. The
	// context bounds only the handshake: the open stream is bounded by ctx below.
	dialCtx, cancelDial := ctx, context.CancelFunc(func() {})
	if timeout := sdk.HTTPClient.Timeout; timeout > 0 {
		dialCtx, cancelDial = context.WithTimeout(ctx, timeout)
	}
	entries, stream, err := sdk.WorkspaceBuildLogsAfter(dialCtx, buildID, afterID)
	cancelDial()
	if err != nil {
		// InputStream turns this into a 504 when the log deadline caused it.
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), w.name)
	}
	reader, writer := io.Pipe()
	stopOnDeadline := context.AfterFunc(ctx, func() {
		_ = stream.Close()
		_ = writer.Close()
	})
	go func() {
		defer func() {
			stopOnDeadline()
			_ = stream.Close()
			_ = writer.Close()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-entries:
				if !ok {
					return
				}
				line := entry.Text() + "\n"
				if int64(len(line)) >= remaining {
					_, _ = io.WriteString(writer, runePrefix(line, remaining))
					return
				}
				if _, err := io.WriteString(writer, line); err != nil {
					return
				}
				remaining -= int64(len(line))
			}
		}
	}()
	return reader, nil
}
