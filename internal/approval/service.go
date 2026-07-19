package approval

import (
	"database/sql"
	"fmt"
	"time"
)

// Service persists pending write actions and runs approved ones.
type Service struct {
	db        *sql.DB
	incidents IncidentFixer
	ttl       time.Duration
}

// NewService uses the shared platform database.
func NewService(db *sql.DB, incidents IncidentFixer) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("approval: platform database is required")
	}
	return &Service{db: db, incidents: incidents, ttl: 24 * time.Hour}, nil
}
