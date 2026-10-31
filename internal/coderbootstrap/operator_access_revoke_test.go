package coderbootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// revokeDriver is a database/sql driver that records each statement and
// reports rowsAffected for it.
type revokeDriver struct {
	rowsAffected int64
	execs        [][]driver.NamedValue
}

func (d *revokeDriver) Connect(context.Context) (driver.Conn, error) { return revokeConn{d}, nil }
func (d *revokeDriver) Driver() driver.Driver                        { return nil }

type revokeConn struct{ d *revokeDriver }

func (revokeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}
func (revokeConn) Close() error              { return nil }
func (revokeConn) Begin() (driver.Tx, error) { return revokeTx{}, nil }
func (c revokeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if !strings.Contains(query, "DELETE FROM api_keys") {
		return nil, errors.New("unexpected statement")
	}
	c.d.execs = append(c.d.execs, args)
	return driver.RowsAffected(c.d.rowsAffected), nil
}

type revokeTx struct{}

func (revokeTx) Commit() error   { return nil }
func (revokeTx) Rollback() error { return nil }

func revokeWith(t *testing.T, rowsAffected int64, req RevokeOperatorTokenRequest) (*revokeDriver, error) {
	t.Helper()
	d := &revokeDriver{rowsAffected: rowsAffected}
	p := NewPostgresOperatorAccessProvisioner()
	p.openDB = func(string) (*sql.DB, error) { return sql.OpenDB(d), nil }
	req.PostgresURL, req.OperatorUsername, req.TokenName = "postgres://example.com/coder", "coder-k8s-operator", "coder-k8s-operator-x"
	return d, p.RevokeOperatorToken(context.Background(), req)
}

// TestRevokeOperatorToken_ByValue checks that a delete by token value removes
// only the row with that ID and secret hash, and that it reports a value that
// names no token, because then a copy of the real token can still work.
func TestRevokeOperatorToken_ByValue(t *testing.T) {
	t.Parallel()

	d, err := revokeWith(t, 1, RevokeOperatorTokenRequest{Token: "AbCdEfGhIj-secret"})
	if err != nil {
		t.Fatalf("revoke by value: %v", err)
	}
	hashed := sha256.Sum256([]byte("secret"))
	if len(d.execs) != 1 || len(d.execs[0]) != 4 || d.execs[0][2].Value != "AbCdEfGhIj" || string(d.execs[0][3].Value.([]byte)) != string(hashed[:]) {
		t.Fatalf("expected one delete with the token ID and secret hash, got %+v", d.execs)
	}

	for _, token := range []string{"AbCdEfGhIj-other", "not-a-token-of-coder", "nodash", "   "} {
		if _, err := revokeWith(t, 0, RevokeOperatorTokenRequest{Token: token}); !errors.Is(err, ErrOperatorTokenNotFound) {
			t.Fatalf("token %q: expected ErrOperatorTokenNotFound, got %v", token, err)
		}
	}

	if _, err := revokeWith(t, 0, RevokeOperatorTokenRequest{}); err != nil {
		t.Fatalf("a delete by name that finds no token must succeed, got %v", err)
	}
}
