package db

import (
	"os"
	"strings"
	"testing"
)

func TestRevocationQueryCopiesStayIdentical(t *testing.T) {
	query, err := os.ReadFile("query/allocation_planning.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(query), "("+RevokedComputerSecretsSQL+") revoked") {
		t.Fatal("allocation planning must use the same durable revocation predicate as admission")
	}
}
