package store

import (
	"database/sql"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/platform/dbschema"
	platformmysql "github.com/dekwanlabs/nasuta/platform/mysql"
)

// OpenMySQL creates the platform-owned pool and installs its schema once.
func OpenMySQL(dsn string) (*sql.DB, error) {
	db, err := platformmysql.Open(dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql store: %w", err)
	}
	if err := dbschema.MigrateMySQL(db, dbschema.AllGroups()...); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql migrate: %w", err)
	}
	return db, nil
}
