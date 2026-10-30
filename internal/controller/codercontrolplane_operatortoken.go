package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

// The record of an operator token is status.operatorTokenSecretRef together
// with status.operatorTokenDatabaseHash. The operator writes it before it can
// create a token, keeps it while the token can still work in Coder, and
// deletes a token only with a record whose database matches the current one.

// recordOperatorToken sets the record in nextStatus and, when the stored
// status does not hold it yet, stores it before EnsureOperatorToken can create
// a token. A stop between the create and the final status write then still
// leaves the record that a later delete needs.
func (r *CoderControlPlaneReconciler) recordOperatorToken(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
	secretName string,
	databaseHash string,
) error {
	if strings.TrimSpace(secretName) == "" {
		return fmt.Errorf("assertion failed: operator token secret name must not be empty")
	}
	ref := &coderv1alpha1.SecretKeySelector{Name: secretName, Key: coderv1alpha1.DefaultTokenSecretKey}
	nextStatus.OperatorTokenSecretRef = ref
	nextStatus.OperatorTokenDatabaseHash = databaseHash

	stored := coderControlPlane.Status.DeepCopy()
	if equality.Semantic.DeepEqual(stored.OperatorTokenSecretRef, ref) && stored.OperatorTokenDatabaseHash == databaseHash {
		return nil
	}
	recorded := stored.DeepCopy()
	recorded.OperatorTokenSecretRef = cloneSecretKeySelector(ref)
	recorded.OperatorTokenDatabaseHash = databaseHash
	// Generation 0 skips the stale-generation guard: the record must be stored
	// before the create even when the spec changed meanwhile.
	if err := r.reconcileStatusForGeneration(ctx, coderControlPlane, *stored, *recorded, 0); err != nil {
		return fmt.Errorf("record operator token before provisioning: %w", err)
	}
	return nil
}

// operatorTokenRevoke is the outcome of revokeRecordedOperatorToken.
type operatorTokenRevoke int

const (
	// operatorTokenNotRecorded: the status records no token, so no token in
	// Coder is provably the operator's, and the operator deletes none.
	operatorTokenNotRecorded operatorTokenRevoke = iota
	// operatorTokenRevoked: the token is gone from the recorded database.
	operatorTokenRevoked
	// operatorTokenRevokePending: the database URL cannot be resolved, or the
	// delete failed. The record stays and the operator retries.
	operatorTokenRevokePending
	// operatorTokenRevokeRefused: the current database URL does not match the
	// recorded database, or the record has no database. The record stays.
	operatorTokenRevokeRefused
)

// revokeRecordedOperatorToken deletes the operator token that the status
// records, and only in the database that the status records. The token name
// is the same in every database, so a delete elsewhere could remove a token
// that the operator did not create there.
func (r *CoderControlPlaneReconciler) revokeRecordedOperatorToken(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
) (operatorTokenRevoke, error) {
	if coderControlPlane.Status.OperatorTokenSecretRef == nil {
		return operatorTokenNotRecorded, nil
	}
	if r.OperatorAccessProvisioner == nil {
		return 0, fmt.Errorf("assertion failed: operator access provisioner must not be nil to delete a recorded operator token")
	}
	tokenName := operatorAccessDatabaseTokenName(coderControlPlane)
	if strings.TrimSpace(tokenName) == "" {
		return 0, fmt.Errorf("assertion failed: operator token name must not be empty")
	}

	postgresURL, err := r.resolveBootstrapPostgresURL(ctx, coderControlPlane)
	if err != nil {
		//nolint:nilerr // an unresolvable URL keeps the record, and the caller retries.
		return operatorTokenRevokePending, nil
	}
	recordedDatabase := coderControlPlane.Status.OperatorTokenDatabaseHash
	if recordedDatabase == "" || recordedDatabase != operatorTokenDatabaseHash(postgresURL) {
		log.FromContext(ctx).Info("Not deleting the operator token: cannot confirm that the control plane still uses the database where the operator created it",
			"tokenName", tokenName)
		return operatorTokenRevokeRefused, nil
	}
	if err := r.OperatorAccessProvisioner.RevokeOperatorToken(ctx, coderbootstrap.RevokeOperatorTokenRequest{
		PostgresURL:      postgresURL,
		OperatorUsername: defaultOperatorAccessUsername,
		TokenName:        tokenName,
	}); err != nil {
		//nolint:nilerr // a failed delete keeps the record, and the caller retries.
		return operatorTokenRevokePending, nil
	}
	return operatorTokenRevoked, nil
}

// operatorTokenDatabaseHash returns the SHA-256 hex hash of postgresURL
// without its password, or "" when postgresURL is neither a postgres:// or
// postgresql:// URL nor a key=value connection string. A key=value string has
// the hash of the postgres:// URL with the same parts. The password is left
// out so that the status reveals nothing about it and a new password keeps the
// hash.
func operatorTokenDatabaseHash(postgresURL string) string {
	trimmed := strings.TrimSpace(postgresURL)
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		if parsed = keywordValueAsURL(trimmed); parsed == nil {
			return ""
		}
	}
	if parsed.User != nil {
		parsed.User = url.User(parsed.User.Username())
	}
	query := parsed.Query()
	query.Del("password")
	parsed.RawQuery = query.Encode()
	sum := sha256.Sum256([]byte(parsed.String()))
	return hex.EncodeToString(sum[:])
}

// keywordValueAsURL returns the postgres:// URL with the parts of the
// key=value connection string dsn, or nil when lib/pq would not read dsn as a
// key=value string.
func keywordValueAsURL(dsn string) *url.URL {
	// lib/pq reads a string with these prefixes as a URL.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return nil
	}
	options, ok := parseKeywordValue(dsn)
	if !ok || len(options) == 0 {
		return nil
	}
	parsed := &url.URL{Scheme: "postgres", Host: options["host"]}
	if port, ok := options["port"]; ok {
		parsed.Host = net.JoinHostPort(options["host"], port)
	}
	if user, ok := options["user"]; ok {
		parsed.User = url.User(user)
	}
	if dbname, ok := options["dbname"]; ok {
		parsed.Path = "/" + dbname
	}
	query := url.Values{}
	for key, value := range options {
		switch key {
		case "host", "port", "user", "dbname":
		default:
			query.Set(key, value)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed
}

// parseKeywordValue splits a key=value connection string by the rules of
// lib/pq's parseOpts (vendor/github.com/lib/pq/conn.go), which the
// provisioner uses to connect: a key ends at a space or "=", a value is bare
// or in single quotes, a backslash escapes the next character, and a later key
// wins. It reports false where lib/pq returns an error.
func parseKeywordValue(dsn string) (map[string]string, bool) {
	options := map[string]string{}
	runes := []rune(dsn)
	next := 0
	skipSpaces := func() {
		for next < len(runes) && unicode.IsSpace(runes[next]) {
			next++
		}
	}
	for {
		skipSpaces()
		if next == len(runes) {
			return options, true
		}
		start := next
		for next < len(runes) && !unicode.IsSpace(runes[next]) && runes[next] != '=' {
			next++
		}
		key := string(runes[start:next])
		skipSpaces()
		if next == len(runes) || runes[next] != '=' {
			return nil, false
		}
		next++
		skipSpaces()

		quoted := next < len(runes) && runes[next] == '\''
		if quoted {
			next++
		}
		var value []rune
		for {
			if next == len(runes) {
				if quoted {
					return nil, false
				}
				break
			}
			char := runes[next]
			if quoted && char == '\'' {
				next++
				break
			}
			if !quoted && unicode.IsSpace(char) {
				break
			}
			if char == '\\' {
				next++
				if next == len(runes) {
					return nil, false
				}
				char = runes[next]
			}
			value = append(value, char)
			next++
		}
		options[key] = string(value)
	}
}
