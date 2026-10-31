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

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

// The record of an operator token is status.operatorTokenSecretRef together
// with status.operatorTokenDatabaseHash, and it has one of two kinds:
//   - an ID record also holds status.operatorTokenID. The operator writes it
//     before the create, so the create may not have happened, and it deletes
//     only the key with that ID.
//   - a confirmed record has no ID. Earlier versions wrote it only after a
//     successful create, so it authorizes the delete by name, as in #401.
//
// The operator keeps a record while its token can still work in Coder, and
// deletes only with a record whose database matches the current one.

// recordOperatorToken sets the record in nextStatus and returns the key ID for
// EnsureOperatorToken. When the stored status has no record for this database
// yet, it first stores an ID record, so that a stop between the create and the
// final status write still leaves the record that a later delete needs.
func (r *CoderControlPlaneReconciler) recordOperatorToken(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
	secretName string,
	databaseHash string,
) (string, error) {
	if strings.TrimSpace(secretName) == "" {
		return "", fmt.Errorf("assertion failed: operator token secret name must not be empty")
	}
	ref := &coderv1alpha1.SecretKeySelector{Name: secretName, Key: coderv1alpha1.DefaultTokenSecretKey}
	nextStatus.OperatorTokenSecretRef = ref
	nextStatus.OperatorTokenDatabaseHash = databaseHash

	// Check the API server, not the cache: a cached status can still show a
	// record that a delete already removed.
	statusReader := r.APIReader
	if statusReader == nil {
		statusReader = r.Client
	}
	key := client.ObjectKeyFromObject(coderControlPlane)
	tokenID := ""
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &coderv1alpha1.CoderControlPlane{}
		if err := statusReader.Get(ctx, key, latest); err != nil {
			return err
		}
		if latest.UID != coderControlPlane.UID {
			return fmt.Errorf("control plane %s has UID %q, not %q", key, latest.UID, coderControlPlane.UID)
		}
		stored := latest.Status
		tokenID = stored.OperatorTokenID
		if stored.OperatorTokenSecretRef != nil && stored.OperatorTokenDatabaseHash == databaseHash {
			nextStatus.OperatorTokenID = tokenID
			if tokenID != "" {
				return nil
			}
			// A confirmed record already authorizes the delete by name in this
			// database, which covers the create. Its new ID reaches the status
			// only after the create succeeded.
			generated, err := coderbootstrap.NewOperatorTokenID()
			tokenID = generated
			return err
		}
		if tokenID == "" {
			generated, err := coderbootstrap.NewOperatorTokenID()
			if err != nil {
				return err
			}
			tokenID = generated
		}
		// A published reference keeps its name until the final status write,
		// so that consumers never read a Secret that holds no token yet.
		if stored.OperatorTokenSecretRef == nil {
			latest.Status.OperatorTokenSecretRef = cloneSecretKeySelector(ref)
		}
		latest.Status.OperatorTokenDatabaseHash = databaseHash
		latest.Status.OperatorTokenID = tokenID
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}
		nextStatus.OperatorTokenID = tokenID
		return nil
	}); err != nil {
		return "", fmt.Errorf("record operator token before provisioning: %w", err)
	}
	if tokenID == "" {
		return "", fmt.Errorf("assertion failed: operator token ID must not be empty")
	}
	return tokenID, nil
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
		TokenID:          coderControlPlane.Status.OperatorTokenID,
	}); err != nil {
		//nolint:nilerr // a failed delete keeps the record, and the caller retries.
		return operatorTokenRevokePending, nil
	}
	return operatorTokenRevoked, nil
}

// operatorTokenDatabaseHash returns the SHA-256 hex hash of postgresURL
// without its password, or "" when postgresURL is neither a postgres:// or
// postgresql:// URL nor a key=value connection string that keywordValueAsURL
// accepts. A key=value string has the hash of the postgres:// URL with the
// same parts. The password is left out so that the status reveals nothing
// about it and a new password keeps the hash.
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
	// lib/pq takes a missing host, port or database name from PGHOST, PGPORT
	// or PGDATABASE of the operator, so only a string that sets them names
	// the database. A URL cannot hold an empty value: lib/pq drops it, but
	// takes an empty key=value option as is.
	for _, key := range []string{"host", "port", "dbname"} {
		if _, ok := options[key]; !ok {
			return nil
		}
	}
	for key, value := range options {
		if value == "" && key != "password" {
			return nil
		}
	}
	parsed := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(options["host"], options["port"]), Path: "/" + options["dbname"]}
	if user, ok := options["user"]; ok {
		parsed.User = url.User(user)
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
