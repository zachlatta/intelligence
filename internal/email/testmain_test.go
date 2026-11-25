package email

import (
	"os"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
)

// TestMain migrates one template database and clones it for each test database.
func TestMain(m *testing.M) {
	os.Exit(dbtest.RunWithTemplate(m))
}
