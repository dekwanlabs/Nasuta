package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/dbschema"
	_ "github.com/go-sql-driver/mysql"
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
		if dsn == "" {
			mysqlErr = fmt.Errorf("MySQL DSN is empty")
			return
		}
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			mysqlErr = fmt.Errorf("mysql open: %w", err)
			return
		}
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(5 * time.Minute)
		db.SetConnMaxIdleTime(2 * time.Minute)

		if err := db.Ping(); err != nil {
			db.Close()
			mysqlErr = fmt.Errorf("mysql ping: %w", err)
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
