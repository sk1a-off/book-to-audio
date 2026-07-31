package migrations

import (
	"strings"
	"testing"
)

func TestMigrationVersion(t *testing.T) {
	t.Parallel()

	version, err := migrationVersion("000123_example.up.sql")
	if err != nil {
		t.Fatalf("migrationVersion() error = %v", err)
	}
	if version != 123 {
		t.Fatalf("migrationVersion() = %d, want 123", version)
	}
}

func TestMigrationVersionRejectsInvalidName(t *testing.T) {
	t.Parallel()

	for _, filename := range []string{"migration.sql", "zero_thing.up.sql", "000000_bad.up.sql"} {
		if _, err := migrationVersion(filename); err == nil {
			t.Errorf("migrationVersion(%q) error = nil", filename)
		}
	}
}

func TestMigrationBodyRemovesTransactionWrapper(t *testing.T) {
	t.Parallel()

	body, err := migrationBody("BEGIN;\nSELECT 1;\nCOMMIT;\n")
	if err != nil {
		t.Fatalf("migrationBody() error = %v", err)
	}
	if strings.TrimSpace(body) != "SELECT 1;" {
		t.Fatalf("migrationBody() = %q", body)
	}
}

func TestMigrationBodyRequiresTransactionWrapper(t *testing.T) {
	t.Parallel()

	if _, err := migrationBody("SELECT 1;"); err == nil {
		t.Fatal("migrationBody() error = nil")
	}
}
