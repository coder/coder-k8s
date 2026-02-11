package coderbootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq" // register PostgreSQL driver for database/sql
)

const (
	operatorTokenIDLength     = 10
	operatorTokenSecretLength = 22
	operatorTokenCharset      = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// EnsureOperatorTokenRequest defines the input required to provision operator
// access directly in the coderd backing PostgreSQL database.
type EnsureOperatorTokenRequest struct {
	PostgresURL      string
	OperatorUsername string
	OperatorEmail    string
	TokenName        string
	TokenLifetime    time.Duration
	ExistingToken    string
}

// RevokeOperatorTokenRequest defines the input required to revoke the managed
// operator token from coderd's PostgreSQL database.
type RevokeOperatorTokenRequest struct {
	PostgresURL      string
	OperatorUsername string
	TokenName        string
}

// OperatorAccessProvisioner provisions and revokes operator access credentials
// for coderd.
type OperatorAccessProvisioner interface {
	EnsureOperatorToken(context.Context, EnsureOperatorTokenRequest) (string, error)
	RevokeOperatorToken(context.Context, RevokeOperatorTokenRequest) error
}

// PostgresOperatorAccessProvisioner provisions operator access credentials by
// connecting directly to coderd's PostgreSQL database.
type PostgresOperatorAccessProvisioner struct {
	openDB func(string) (*sql.DB, error)
	now    func() time.Time
}

// NewPostgresOperatorAccessProvisioner returns a PostgreSQL-backed operator
// access provisioner.
func NewPostgresOperatorAccessProvisioner() *PostgresOperatorAccessProvisioner {
	return &PostgresOperatorAccessProvisioner{
		openDB: openPostgresDatabase,
		now:    time.Now,
	}
}

func openPostgresDatabase(postgresURL string) (*sql.DB, error) {
	if strings.TrimSpace(postgresURL) == "" {
		return nil, fmt.Errorf("assertion failed: postgres URL must not be empty")
	}

	db, err := sql.Open("postgres", postgresURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres connection: %w", err)
	}
	if db == nil {
		return nil, fmt.Errorf("assertion failed: sql.Open returned nil db and nil error")
	}

	return db, nil
}

// EnsureOperatorToken ensures the operator system user exists, grants
// organization-admin membership in all organizations, reuses the provided
// existing token when still valid, and otherwise rotates the token with the
// configured token name.
func (p *PostgresOperatorAccessProvisioner) EnsureOperatorToken(ctx context.Context, req EnsureOperatorTokenRequest) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("assertion failed: context must not be nil")
	}
	if err := req.validate(); err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("assertion failed: provisioner must not be nil")
	}
	if p.openDB == nil {
		return "", fmt.Errorf("assertion failed: provisioner openDB must not be nil")
	}
	if p.now == nil {
		return "", fmt.Errorf("assertion failed: provisioner now clock must not be nil")
	}

	db, err := p.openDB(req.PostgresURL)
	if err != nil {
		return "", fmt.Errorf("open coderd postgres database: %w", err)
	}
	if db == nil {
		return "", fmt.Errorf("assertion failed: openDB returned nil db and nil error")
	}
	defer func() {
		_ = db.Close()
	}()

	if pingErr := db.PingContext(ctx); pingErr != nil {
		return "", fmt.Errorf("ping coderd postgres database: %w", pingErr)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin operator access transaction: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		_ = tx.Rollback()
	}()

	now := p.now().UTC()
	if now.IsZero() {
		return "", fmt.Errorf("assertion failed: provisioner clock returned zero time")
	}

	userID, err := ensureOperatorUser(ctx, tx, now, req)
	if err != nil {
		return "", err
	}
	if err := ensureOperatorMemberships(ctx, tx, now, userID); err != nil {
		return "", err
	}

	token, err := ensureOperatorToken(ctx, tx, now, userID, req)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("assertion failed: ensured operator token must not be empty")
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit operator access transaction: %w", err)
	}
	committed = true

	return token, nil
}

// RevokeOperatorToken deletes the managed operator API token when present.
func (p *PostgresOperatorAccessProvisioner) RevokeOperatorToken(ctx context.Context, req RevokeOperatorTokenRequest) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}
	if err := req.validate(); err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("assertion failed: provisioner must not be nil")
	}
	if p.openDB == nil {
		return fmt.Errorf("assertion failed: provisioner openDB must not be nil")
	}

	db, err := p.openDB(req.PostgresURL)
	if err != nil {
		return fmt.Errorf("open coderd postgres database: %w", err)
	}
	if db == nil {
		return fmt.Errorf("assertion failed: openDB returned nil db and nil error")
	}
	defer func() {
		_ = db.Close()
	}()

	if pingErr := db.PingContext(ctx); pingErr != nil {
		return fmt.Errorf("ping coderd postgres database: %w", pingErr)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operator access revoke transaction: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		_ = tx.Rollback()
	}()

	if err := revokeOperatorToken(ctx, tx, req); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operator access revoke transaction: %w", err)
	}
	committed = true

	return nil
}

func (r EnsureOperatorTokenRequest) validate() error {
	if strings.TrimSpace(r.PostgresURL) == "" {
		return fmt.Errorf("operator access postgres URL is required")
	}
	if strings.TrimSpace(r.OperatorUsername) == "" {
		return fmt.Errorf("operator access username is required")
	}
	if strings.TrimSpace(r.OperatorEmail) == "" {
		return fmt.Errorf("operator access email is required")
	}
	if strings.TrimSpace(r.TokenName) == "" {
		return fmt.Errorf("operator access token name is required")
	}
	if r.TokenLifetime <= 0 {
		return fmt.Errorf("operator access token lifetime must be positive")
	}

	return nil
}

func (r RevokeOperatorTokenRequest) validate() error {
	if strings.TrimSpace(r.PostgresURL) == "" {
		return fmt.Errorf("operator access postgres URL is required")
	}
	if strings.TrimSpace(r.OperatorUsername) == "" {
		return fmt.Errorf("operator access username is required")
	}
	if strings.TrimSpace(r.TokenName) == "" {
		return fmt.Errorf("operator access token name is required")
	}

	return nil
}

func ensureOperatorUser(ctx context.Context, tx *sql.Tx, now time.Time, req EnsureOperatorTokenRequest) (uuid.UUID, error) {
	if tx == nil {
		return uuid.Nil, fmt.Errorf("assertion failed: transaction must not be nil")
	}

	const lookupOperatorUserQuery = `
SELECT id
FROM users
WHERE deleted = false
  AND lower(username) = lower($1)
LIMIT 1
`

	var userID uuid.UUID
	err := tx.QueryRowContext(ctx, lookupOperatorUserQuery, req.OperatorUsername).Scan(&userID)
	switch {
	case err == nil:
		// Existing user found.
	case errors.Is(err, sql.ErrNoRows):
		userID = uuid.New()
		const insertOperatorUserQuery = `
INSERT INTO users (
	id,
	email,
	username,
	name,
	hashed_password,
	created_at,
	updated_at,
	rbac_roles,
	login_type,
	status,
	is_system
)
VALUES (
	$1,
	$2,
	$3,
	$4,
	$5,
	$6,
	$6,
	ARRAY['owner'],
	'none'::login_type,
	'active'::user_status,
	true
)
`
		if _, execErr := tx.ExecContext(
			ctx,
			insertOperatorUserQuery,
			userID,
			req.OperatorEmail,
			req.OperatorUsername,
			req.OperatorUsername,
			[]byte("none"),
			now,
		); execErr != nil {
			return uuid.Nil, fmt.Errorf("insert operator user %q: %w", req.OperatorUsername, execErr)
		}
	default:
		return uuid.Nil, fmt.Errorf("query operator user %q: %w", req.OperatorUsername, err)
	}

	if userID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("assertion failed: operator user ID must not be nil")
	}

	const enforceOperatorUserQuery = `
UPDATE users
SET
	email = $2,
	name = $3,
	hashed_password = $4,
	updated_at = $5,
	rbac_roles = ARRAY['owner'],
	login_type = 'none'::login_type,
	status = 'active'::user_status,
	is_system = true
WHERE id = $1
`
	result, err := tx.ExecContext(
		ctx,
		enforceOperatorUserQuery,
		userID,
		req.OperatorEmail,
		req.OperatorUsername,
		[]byte("none"),
		now,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("enforce operator user %q attributes: %w", req.OperatorUsername, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return uuid.Nil, fmt.Errorf("get rows affected while enforcing operator user %q: %w", req.OperatorUsername, err)
	}
	if rowsAffected != 1 {
		return uuid.Nil, fmt.Errorf("assertion failed: expected one operator user row updated, got %d", rowsAffected)
	}

	return userID, nil
}

func ensureOperatorMemberships(ctx context.Context, tx *sql.Tx, now time.Time, userID uuid.UUID) error {
	if tx == nil {
		return fmt.Errorf("assertion failed: transaction must not be nil")
	}
	if userID == uuid.Nil {
		return fmt.Errorf("assertion failed: operator user ID must not be nil")
	}

	const listOrganizationsQuery = `
SELECT id
FROM organizations
WHERE deleted = false
`
	rows, err := tx.QueryContext(ctx, listOrganizationsQuery)
	if err != nil {
		return fmt.Errorf("list organizations: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	const upsertOrganizationMemberQuery = `
INSERT INTO organization_members (
	organization_id,
	user_id,
	created_at,
	updated_at,
	roles
)
VALUES (
	$1,
	$2,
	$3,
	$3,
	ARRAY['organization-admin']
)
ON CONFLICT (organization_id, user_id) DO UPDATE
SET
	updated_at = EXCLUDED.updated_at,
	roles = ARRAY['organization-admin']
`

	organizationCount := 0
	for rows.Next() {
		organizationCount++

		var organizationID uuid.UUID
		if err := rows.Scan(&organizationID); err != nil {
			return fmt.Errorf("scan organization ID: %w", err)
		}
		if organizationID == uuid.Nil {
			return fmt.Errorf("assertion failed: organization ID must not be nil")
		}

		if _, execErr := tx.ExecContext(ctx, upsertOrganizationMemberQuery, organizationID, userID, now); execErr != nil {
			return fmt.Errorf("upsert operator organization membership for org %q: %w", organizationID.String(), execErr)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate organizations: %w", err)
	}
	if organizationCount == 0 {
		return fmt.Errorf("assertion failed: expected at least one organization")
	}

	return nil
}

func ensureOperatorToken(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	userID uuid.UUID,
	req EnsureOperatorTokenRequest,
) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("assertion failed: transaction must not be nil")
	}
	if userID == uuid.Nil {
		return "", fmt.Errorf("assertion failed: operator user ID must not be nil")
	}

	existingToken := strings.TrimSpace(req.ExistingToken)
	if existingToken != "" {
		tokenStillValid, err := existingOperatorTokenStillValid(ctx, tx, now, userID, req.TokenName, existingToken)
		if err != nil {
			return "", err
		}
		if tokenStillValid {
			return existingToken, nil
		}
	}

	return rotateOperatorToken(ctx, tx, now, userID, req)
}

func existingOperatorTokenStillValid(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	userID uuid.UUID,
	tokenName string,
	existingToken string,
) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("assertion failed: transaction must not be nil")
	}
	if userID == uuid.Nil {
		return false, fmt.Errorf("assertion failed: operator user ID must not be nil")
	}
	if strings.TrimSpace(tokenName) == "" {
		return false, fmt.Errorf("assertion failed: token name must not be empty")
	}
	if strings.TrimSpace(existingToken) == "" {
		return false, fmt.Errorf("assertion failed: existing token must not be empty")
	}

	tokenID, tokenSecret, validFormat := splitOperatorToken(existingToken)
	if !validFormat {
		return false, nil
	}

	hashedSecret := sha256.Sum256([]byte(tokenSecret))

	// #nosec G101 -- token_name here is a column identifier, not a credential.
	const lookupExistingTokenQuery = `
SELECT expires_at
FROM api_keys
WHERE id = $1
  AND user_id = $2
  AND login_type = 'token'::login_type
  AND token_name = $3
  AND hashed_secret = $4
LIMIT 1
`
	var expiresAt time.Time
	err := tx.QueryRowContext(ctx, lookupExistingTokenQuery, tokenID, userID, tokenName, hashedSecret[:]).Scan(&expiresAt)
	switch {
	case err == nil:
		if !expiresAt.After(now) {
			return false, nil
		}
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("query existing operator token %q: %w", tokenName, err)
	}
}

func splitOperatorToken(token string) (string, string, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", "", false
	}

	tokenID, tokenSecret, found := strings.Cut(token, "-")
	if !found {
		return "", "", false
	}
	if tokenID == "" || tokenSecret == "" {
		return "", "", false
	}

	return tokenID, tokenSecret, true
}

func rotateOperatorToken(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	userID uuid.UUID,
	req EnsureOperatorTokenRequest,
) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("assertion failed: transaction must not be nil")
	}
	if userID == uuid.Nil {
		return "", fmt.Errorf("assertion failed: operator user ID must not be nil")
	}

	// #nosec G101 -- token_name here is a column identifier, not a credential.
	const deleteExistingTokenQuery = `
DELETE FROM api_keys
WHERE user_id = $1
  AND login_type = 'token'::login_type
  AND token_name = $2
`
	if _, err := tx.ExecContext(ctx, deleteExistingTokenQuery, userID, req.TokenName); err != nil {
		return "", fmt.Errorf("delete existing operator token %q: %w", req.TokenName, err)
	}

	tokenID, err := randomTokenPart(operatorTokenIDLength)
	if err != nil {
		return "", fmt.Errorf("generate operator token ID: %w", err)
	}
	tokenSecret, err := randomTokenPart(operatorTokenSecretLength)
	if err != nil {
		return "", fmt.Errorf("generate operator token secret: %w", err)
	}

	hashedSecret := sha256.Sum256([]byte(tokenSecret))
	token := fmt.Sprintf("%s-%s", tokenID, tokenSecret)

	lifetimeSeconds := int64(req.TokenLifetime.Seconds())
	if lifetimeSeconds <= 0 {
		return "", fmt.Errorf("assertion failed: token lifetime seconds must be positive")
	}

	expiresAt := now.Add(req.TokenLifetime).UTC()

	// #nosec G101 -- token_name here is a column identifier, not a credential.
	const insertOperatorTokenQuery = `
INSERT INTO api_keys (
	id,
	hashed_secret,
	user_id,
	last_used,
	expires_at,
	created_at,
	updated_at,
	login_type,
	lifetime_seconds,
	ip_address,
	token_name,
	scopes,
	allow_list
)
VALUES (
	$1,
	$2,
	$3,
	$4,
	$5,
	$4,
	$4,
	'token'::login_type,
	$6,
	'0.0.0.0'::inet,
	$7,
	ARRAY['coder:all']::api_key_scope[],
	ARRAY['*:*']
)
`
	if _, err := tx.ExecContext(
		ctx,
		insertOperatorTokenQuery,
		tokenID,
		hashedSecret[:],
		userID,
		now,
		expiresAt,
		lifetimeSeconds,
		req.TokenName,
	); err != nil {
		return "", fmt.Errorf("insert operator token %q: %w", req.TokenName, err)
	}

	return token, nil
}

func revokeOperatorToken(ctx context.Context, tx *sql.Tx, req RevokeOperatorTokenRequest) error {
	if tx == nil {
		return fmt.Errorf("assertion failed: transaction must not be nil")
	}
	if err := req.validate(); err != nil {
		return err
	}

	// #nosec G101 -- token_name here is a column identifier, not a credential.
	const deleteOperatorTokenByNameQuery = `
DELETE FROM api_keys
WHERE login_type = 'token'::login_type
  AND token_name = $2
  AND user_id IN (
	SELECT id
	FROM users
	WHERE deleted = false
	  AND lower(username) = lower($1)
)
`
	if _, err := tx.ExecContext(ctx, deleteOperatorTokenByNameQuery, req.OperatorUsername, req.TokenName); err != nil {
		return fmt.Errorf("delete operator token %q: %w", req.TokenName, err)
	}

	return nil
}

func randomTokenPart(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("assertion failed: token part length must be positive")
	}
	if len(operatorTokenCharset) == 0 {
		return "", fmt.Errorf("assertion failed: token charset must not be empty")
	}

	output := make([]byte, length)
	charsetLength := byte(len(operatorTokenCharset))
	rejectionThreshold := byte(255 - (256 % int(charsetLength)))

	index := 0
	for index < length {
		var randomByte [1]byte
		if _, err := rand.Read(randomByte[:]); err != nil {
			return "", fmt.Errorf("read random byte: %w", err)
		}
		if randomByte[0] > rejectionThreshold {
			continue
		}

		output[index] = operatorTokenCharset[int(randomByte[0])%len(operatorTokenCharset)]
		index++
	}

	return string(output), nil
}
