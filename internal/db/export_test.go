package db

// Exported for the db_test package's tests.
var (
	MigrateFrom       = migrateFrom
	MigrationOrder    = migrationOrder
	ShippedMigrations = func() ([]string, error) { return migrationNames(migrationDir()) }
)
