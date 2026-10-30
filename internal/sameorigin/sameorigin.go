// Package sameorigin keeps credentials on the origin they were meant for.
//
// net/http copies custom request headers, such as Coder-Session-Token, to every
// redirect target. It strips only Authorization, Cookie and similar headers on
// a cross-domain hop. Every Coder client that sends the operator token sets
// RedirectPolicy as its CheckRedirect, so a redirect to another origin fails
// instead of sending the token there.
//
// codersdk dials websockets with a new http.Client that copies only the
// Transport, so CheckRedirect does not apply there. A client that can dial a
// websocket also wraps its Transport with RoundTripper, which refuses every
// request that leaves the origin.
package sameorigin

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// MaxRedirects matches the limit of net/http's default redirect policy.
const MaxRedirects = 10

// ErrCrossOrigin is the error that RedirectPolicy returns for a redirect to a
// different origin. The message does not name the redirect target. net/http
// still wraps it in a *url.Error that names the target.
var ErrCrossOrigin = errors.New("refusing redirect to a different origin than the coder URL")

// ErrCrossOriginRequest is the error that RoundTripper returns for a request to
// a different origin. The request can be a redirect that a client without
// RedirectPolicy follows, or a direct call, so the message does not say
// "redirect". net/http wraps it in a *url.Error that names the target.
var ErrCrossOriginRequest = errors.New("refusing request to a different origin than the coder URL")

// RedirectPolicy returns a CheckRedirect function that follows a redirect only
// when it stays on the origin of origin, and stops after MaxRedirects.
func RedirectPolicy(origin *url.URL) func(*http.Request, []*http.Request) error {
	if origin == nil {
		panic("assertion failed: redirect policy origin must not be nil")
	}
	return func(req *http.Request, via []*http.Request) error {
		if req == nil || req.URL == nil {
			return errors.New("assertion failed: redirect request URL must not be nil")
		}
		if len(via) >= MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", MaxRedirects)
		}
		if !Same(req.URL, origin) {
			return ErrCrossOrigin
		}
		return nil
	}
}

// Same compares scheme, hostname and effective port, so that "https://host"
// and "https://host:443" are the same origin.
func Same(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		sameHostname(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

// Key returns the same string for two URLs exactly when Same reports one origin,
// so callers can index per-origin state by it.
func Key(u *url.URL) string {
	host, zone, hasZone := strings.Cut(u.Hostname(), "%")
	lower := []byte(host)
	for i := range lower {
		lower[i] = asciiLower(lower[i])
	}
	host = string(lower)
	if hasZone {
		host += "%" + zone
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(host, effectivePort(u))
}

// sameHostname folds ASCII case for the host but compares an IPv6 zone
// exactly, because interface names are case-sensitive. Unicode case folding
// is unsafe here: "σ" and "ς" fold equal but map to different IDNA hosts.
func sameHostname(a, b string) bool {
	aHost, aZone, _ := strings.Cut(a, "%")
	bHost, bZone, _ := strings.Cut(b, "%")
	return asciiEqualFold(aHost, bHost) && aZone == bZone
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLower(a[i]) != asciiLower(b[i]) {
			return false
		}
	}
	return true
}

func asciiLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// RoundTripper returns an http.RoundTripper that sends a request through base
// only when the request URL is on the origin of origin. It returns
// ErrCrossOriginRequest for every other request, so a redirect that a caller follows
// without RedirectPolicy still cannot send the token to another origin.
func RoundTripper(origin *url.URL, base http.RoundTripper) http.RoundTripper {
	if origin == nil {
		panic("assertion failed: round tripper origin must not be nil")
	}
	if base == nil {
		panic("assertion failed: round tripper base must not be nil")
	}
	originCopy := *origin
	return &roundTripper{origin: &originCopy, base: base}
}

type roundTripper struct {
	origin *url.URL
	base   http.RoundTripper
}

func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("assertion failed: round trip request URL must not be nil")
	}
	if !Same(req.URL, t.origin) {
		// The RoundTripper contract requires closing the body, even on error.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrCrossOriginRequest
	}
	return t.base.RoundTrip(req)
}

// CloseIdleConnections forwards to base, so http.Client.CloseIdleConnections
// still reaches the shared transport.
func (t *roundTripper) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
