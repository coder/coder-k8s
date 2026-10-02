package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"unicode/utf8"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/warning"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

const logContentType = "text/plain; charset=utf-8"

// WorkspaceLogStorage serves the coderworkspaces/log subresource: the latest build log of a
// workspace as text/plain, in Coder's own text line format.
type WorkspaceLogStorage struct {
	workspaces *WorkspaceStorage
	limits     workspaceLogLimits
	slots      *logSlots
}

var (
	_ rest.Storage           = (*WorkspaceLogStorage)(nil)
	_ rest.GetterWithOptions = (*WorkspaceLogStorage)(nil)
	_ rest.StorageMetadata   = (*WorkspaceLogStorage)(nil)
)

// NewWorkspaceLogStorage returns log storage that resolves workspaces through workspaces.
func NewWorkspaceLogStorage(workspaces *WorkspaceStorage) *WorkspaceLogStorage {
	return newWorkspaceLogStorage(workspaces, defaultWorkspaceLogLimits())
}

func newWorkspaceLogStorage(workspaces *WorkspaceStorage, limits workspaceLogLimits) *WorkspaceLogStorage {
	if workspaces == nil {
		panic("assertion failed: workspace storage must not be nil")
	}
	return &WorkspaceLogStorage{
		workspaces: workspaces,
		limits:     limits,
		slots:      newLogSlots(limits), // validates limits
	}
}

// New returns a CoderWorkspace, as Pod log storage returns a Pod; it is used only for API
// metadata.
func (s *WorkspaceLogStorage) New() runtime.Object {
	return &aggregationv1alpha1.CoderWorkspace{}
}

// Destroy implements rest.Storage.
func (s *WorkspaceLogStorage) Destroy() {}

// NewGetOptions implements rest.GetterWithOptions.
func (s *WorkspaceLogStorage) NewGetOptions() (runtime.Object, bool, string) {
	return &aggregationv1alpha1.CoderWorkspaceLogOptions{}, false, ""
}

// ProducesMIMETypes implements rest.StorageMetadata.
func (s *WorkspaceLogStorage) ProducesMIMETypes(string) []string {
	return []string{"text/plain"}
}

// ProducesObject implements rest.StorageMetadata.
func (s *WorkspaceLogStorage) ProducesObject(string) interface{} {
	return ""
}

// Get validates the request and returns a stream. It makes no Coder call: InputStream takes a
// slot first, so a request over the slot cap never reaches Coder.
func (s *WorkspaceLogStorage) Get(ctx context.Context, name string, opts runtime.Object) (runtime.Object, error) {
	if s == nil || ctx == nil {
		return nil, fmt.Errorf("assertion failed: log storage and context must not be nil")
	}
	options, ok := opts.(*aggregationv1alpha1.CoderWorkspaceLogOptions)
	if !ok || options == nil {
		return nil, fmt.Errorf("assertion failed: unexpected log options type %T", opts)
	}
	if options.LimitBytes != nil && *options.LimitBytes < 1 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: aggregationv1alpha1.SchemeGroupVersion.Group, Kind: "CoderWorkspaceLogOptions"},
			name,
			field.ErrorList{field.Invalid(field.NewPath("limitBytes"), *options.LimitBytes, "must be greater than 0")},
		)
	}
	namespace, err := requiredNamespaceFromRequestContext(ctx)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := coder.ParseWorkspaceName(name); err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid workspace name %q: %v", name, err))
	}
	caller, ok := request.UserFrom(ctx)
	if !ok || caller == nil || caller.GetName() == "" {
		return nil, fmt.Errorf("assertion failed: authenticated log request must carry a user")
	}

	budget := s.limits.maxBytes
	callerLimited := options.LimitBytes != nil && *options.LimitBytes <= budget
	if callerLimited {
		budget = *options.LimitBytes
	}
	// InputStream's context has no namespace value, so the stream captures what it needs here.
	return &workspaceLogStream{
		storage:       s,
		namespace:     namespace,
		name:          name,
		user:          caller.GetName(),
		budget:        budget,
		callerLimited: callerLimited,
		follow:        options.Follow,
	}, nil
}

// workspaceLogStream is the rest.ResourceStreamer for one log request.
type workspaceLogStream struct {
	storage       *WorkspaceLogStorage
	namespace     string
	name          string
	user          string
	budget        int64
	callerLimited bool
	follow        bool
}

var _ rest.ResourceStreamer = (*workspaceLogStream)(nil)

// GetObjectKind implements runtime.Object.
func (w *workspaceLogStream) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

// DeepCopyObject implements runtime.Object.
func (w *workspaceLogStream) DeepCopyObject() runtime.Object {
	c := *w
	return &c
}

// InputStream takes a slot, starts the log deadline, resolves the workspace and reads its
// latest build log. Any failure here becomes a Status, because nothing has been written yet.
func (w *workspaceLogStream) InputStream(ctx context.Context, _, _ string) (io.ReadCloser, bool, string, error) {
	if w == nil || w.storage == nil || ctx == nil {
		return nil, false, "", fmt.Errorf("assertion failed: log stream and context must not be nil")
	}
	release, err := w.storage.slots.acquire(w.user)
	if err != nil {
		return nil, false, "", err
	}
	// One deadline covers resolution and every read from Coder. A follow stream gets the long one.
	duration := w.storage.limits.duration
	if w.follow {
		duration = w.storage.limits.followDuration
	}
	logCtx, cancel := context.WithTimeout(ctx, duration)
	out := &logReadCloser{cancel: cancel, release: release}
	// The deferred Close frees the slot on every return except a successful one, which hands
	// the slot to the response body: StreamObject closes the body after its write loop.
	handedOff := false
	defer func() {
		if !handedOff {
			_ = out.Close()
		}
	}()

	// Once the log deadline has passed, every failure is a 504, whichever Coder call stalled.
	failed := func(err error) error {
		if errors.Is(logCtx.Err(), context.DeadlineExceeded) {
			return apierrors.NewTimeoutError("reading the build log from Coder timed out", 0)
		}
		return err
	}

	sdk, workspace, err := w.storage.workspaces.resolveWorkspace(logCtx, w.namespace, w.name)
	if err != nil {
		return nil, false, "", failed(err)
	}
	if workspace.LatestBuild.ID == uuid.Nil {
		return nil, false, "", fmt.Errorf("assertion failed: workspace %q has no latest build", w.name)
	}
	snapshot, err := w.readSnapshot(logCtx, sdk, workspace.LatestBuild.ID)
	if err != nil {
		return nil, false, "", failed(err)
	}
	for _, msg := range snapshot.warnings {
		warning.AddWarning(ctx, "", msg)
	}
	out.Reader = bytes.NewReader(snapshot.data)
	if !w.follow || snapshot.truncated {
		handedOff = true
		return out, false, logContentType, nil
	}
	live, err := w.followLog(logCtx, sdk, workspace.LatestBuild.ID, snapshot.lastID, w.budget-int64(len(snapshot.data)))
	if err != nil {
		return nil, false, "", failed(err)
	}
	out.Reader = io.MultiReader(out.Reader, live)
	handedOff = true
	return out, true, logContentType, nil
}

// logSnapshot is the rendered existing log of a build.
type logSnapshot struct {
	data     []byte
	lastID   int64 // ID of the last rendered entry; follow continues after it
	warnings []string
	// truncated is set when the byte budget or the read cap ended the snapshot.
	truncated bool
}

// readSnapshot renders the build's existing log entries, bounded by the stream's byte budget
// and the read cap. It never puts log content into an error or warning.
func (w *workspaceLogStream) readSnapshot(ctx context.Context, sdk *codersdk.Client, buildID uuid.UUID) (logSnapshot, error) {
	resource := aggregationv1alpha1.Resource("coderworkspaces")
	res, err := sdk.Request(ctx, http.MethodGet, fmt.Sprintf("/api/v2/workspacebuilds/%s/logs", buildID), nil)
	if err != nil {
		return logSnapshot{}, coder.MapCoderError(err, resource, w.name)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// ReadBodyAsError reads the whole body, so cap what an error response may hold.
		limited := *res
		limited.Body = io.NopCloser(io.LimitReader(res.Body, maxLogErrorBodyBytes))
		return logSnapshot{}, coder.MapCoderError(codersdk.ReadBodyAsError(&limited), resource, w.name)
	}

	limits := w.storage.limits
	scanned := &countingReader{r: io.LimitReader(res.Body, limits.maxScanBytes)}
	decoder := json.NewDecoder(scanned)
	var out bytes.Buffer
	var result logSnapshot
	scanCapHit := func(err error) bool {
		return scanned.n >= limits.maxScanBytes && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF))
	}
	// readFailed reports a failed read without log content: malformed JSON, or a read that broke
	// off. InputStream turns it into a 504 when the log deadline caused it.
	readFailed := func(err error) error {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.Is(err, errMalformedLog) || errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return apierrors.NewInternalError(errMalformedLog)
		}
		// A body read that ran out of time (the client's request timeout) is a 504, as in
		// coder.MapCoderError.
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return apierrors.NewTimeoutError("the Coder API did not answer in time", 0)
		}
		return apierrors.NewInternalError(errors.New("reading the build log from Coder failed"))
	}

	open, err := decoder.Token()
	switch {
	case err != nil:
		return logSnapshot{}, readFailed(err)
	case open == nil: // JSON null: no log entries.
		if err := endOfLog(decoder); err != nil {
			return logSnapshot{}, readFailed(err)
		}
		return logSnapshot{}, nil
	case open != json.Delim('['):
		return logSnapshot{}, readFailed(errMalformedLog)
	}
	scanTruncated := false
	for decoder.More() {
		var entry codersdk.ProvisionerJobLog
		if err := decoder.Decode(&entry); err != nil {
			if scanCapHit(err) {
				// A json.Decoder reports no further error after a failed Decode, so record it here.
				scanTruncated = true
				break
			}
			return logSnapshot{}, readFailed(err)
		}
		line := entry.Text() + "\n"
		remaining := w.budget - int64(out.Len())
		if int64(len(line)) > remaining {
			out.WriteString(runePrefix(line, remaining))
			if !w.callerLimited {
				result.warnings = append(result.warnings, fmt.Sprintf("build log truncated at the server limit of %d bytes", limits.maxBytes))
			}
			result.data, result.truncated = out.Bytes(), true
			return result, nil
		}
		out.WriteString(line)
		result.lastID = entry.ID
	}
	if !scanTruncated {
		if _, err := decoder.Token(); err != nil {
			if !scanCapHit(err) {
				return logSnapshot{}, readFailed(err)
			}
			scanTruncated = true
		} else if err := endOfLog(decoder); err != nil {
			return logSnapshot{}, readFailed(err)
		}
	}
	if scanTruncated {
		result.warnings = append(result.warnings, fmt.Sprintf("build log truncated: Coder returned more than the server read limit of %d bytes", limits.maxScanBytes))
	}
	result.data, result.truncated = out.Bytes(), scanTruncated
	return result, nil
}

// maxLogErrorBodyBytes caps how much of a non-200 Coder response is read.
const maxLogErrorBodyBytes = 64 << 10

// errMalformedLog reports JSON from Coder that is not a build log. It carries no log content.
var errMalformedLog = errors.New("coder returned a malformed build log")

// endOfLog returns nil when only whitespace is left after the top-level JSON value, so trailing
// data after a complete log is rejected instead of silently dropped.
func endOfLog(decoder *json.Decoder) error {
	_, err := decoder.Token()
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err == nil:
		return errMalformedLog
	default:
		return err
	}
}

// runePrefix returns the longest prefix of s that is at most n bytes and does not split a UTF-8
// sequence.
func runePrefix(s string, n int64) string {
	if n >= int64(len(s)) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// countingReader counts bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// logReadCloser is the response body. Close ends the log deadline and frees the slot exactly
// once. StreamObject calls Close only after its write loop returns, so a slot is never freed
// while its response is still being written.
type logReadCloser struct {
	io.Reader
	cancel  context.CancelFunc
	release func()
	once    sync.Once
}

func (l *logReadCloser) Close() error {
	l.once.Do(func() {
		l.cancel()
		l.release()
	})
	return nil
}
