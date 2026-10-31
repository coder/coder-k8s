package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOperatorTokenDatabaseHash pins which PostgreSQL connection strings name
// the same database for the delete of the operator token: a key=value string
// has the hash of the URL with the same parts, and no password counts.
func TestOperatorTokenDatabaseHash(t *testing.T) {
	sum := sha256.Sum256([]byte("postgres://coder@db.example:5432/coder?sslmode=disable"))
	urlHash := hex.EncodeToString(sum[:])
	require.Equal(t, urlHash, operatorTokenDatabaseHash("postgres://coder:secret@db.example:5432/coder?sslmode=disable"),
		"the hash of a URL must not change, because statuses that earlier versions wrote hold it")

	for _, dsn := range []string{
		"host=db.example port=5432 user=coder password=secret dbname=coder sslmode=disable",
		" sslmode = disable dbname=coder user='coder' password='a \\'quoted\\' secret' port=5432 host=db.example ",
		"host=db.example port=5432 user=coder dbname=coder sslmode=disable",
	} {
		require.Equal(t, urlHash, operatorTokenDatabaseHash(dsn), dsn)
	}

	seen := map[string]string{urlHash: "the URL"}
	for _, dsn := range []string{
		"host=other.example port=5432 user=coder dbname=coder sslmode=disable",
		"host=db.example port=5433 user=coder dbname=coder sslmode=disable",
		"host=db.example port=5432 user=admin dbname=coder sslmode=disable",
		"host=db.example port=5432 user=coder dbname=other sslmode=disable",
		"host=db.example port=5432 user=coder dbname=coder sslmode=require",
		"host=db.example port=5432 user=coder dbname=coder",
		"host=/var/run/postgresql port=5432 user=coder dbname=coder",
	} {
		hash := operatorTokenDatabaseHash(dsn)
		require.NotEmpty(t, hash, dsn)
		require.NotContains(t, seen, hash, "%q names another database than %q", dsn, seen[hash])
		seen[hash] = dsn
	}

	// lib/pq takes a missing host, port or database name from PGHOST, PGPORT
	// or PGDATABASE, so the string alone does not name the database. It takes
	// an empty value as is, which a URL cannot hold.
	for _, unusable := range []string{
		"", "  ", "host=db.example dbname", "host='db.example", "host=db.example\\", "mysql://db.example/coder",
		"host='' port=5432 user=coder dbname=coder", "host=db.example port=5432 dbname=coder user=",
		"user=coder dbname=coder", "host=db.example user=coder dbname=coder", "host=db.example port=5432 user=coder",
	} {
		require.Empty(t, operatorTokenDatabaseHash(unusable), "%q", unusable)
	}
}
