// Package sameorigin keeps credentials on the origin they were meant for.
//
// net/http copies custom request headers, such as Coder-Session-Token, to every
// redirect target. It strips only Authorization, Cookie and similar headers on
// a cross-domain hop. Every Coder client that sends the operator token sets
// RedirectPolicy as its CheckRedirect, so a redirect to another origin fails
// instead of sending the token there.
package sameorigin

import (
	"errors"
	"fmt"
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
