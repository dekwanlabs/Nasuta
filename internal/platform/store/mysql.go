package store

import (
	"database/sql"
	"fmt"
	"sync"

	"github.com/dekwanlabs/nasuta/internal/platform/dbschema"
	platformmysql "github.com/dekwanlabs/nasuta/platform/mysql"
)

var (
	mysqlDB   *sql.DB
	mysqlOnce sync.Once
	mysqlErr  error
)

// MySQL returns the singleton MySQL connection pool.
// Migrations run when the pool is first created.
// Callers must treat the returned pool as shared infrastructure and not close it.
func MySQL(dsn string) (*sql.DB, error) {
	mysqlOnce.Do(func() {
		db, err := platformmysql.Open(dsn)
		if err != nil {
			mysqlErr = fmt.Errorf("mysql store: %w", err)
			return
		}
		// Run all known schema migrations once.
		for _, g := range dbschema.AllGroups() {
			if err := dbschema.MigrateMySQL(db, g); err != nil {
				db.Close()
				mysqlErr = fmt.Errorf("mysql migrate %s: %w", g, err)
				return
			}
		}
		mysqlDB = db
	})
	return mysqlDB, mysqlErr
}
