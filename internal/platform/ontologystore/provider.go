package ontologystore

import (
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	ontologysqlite "github.com/dekwanlabs/nasuta/internal/platform/ontologystore/sqlite"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// New is the only production dispatcher for ontology providers.
func New(cfg config.OntologyConfig, structure *store.SQLite) (ontology.Backend, error) {
	switch cfg.Provider {
	case "sqlite":
		if structure == nil {
			return nil, fmt.Errorf("ontology provider %q requires the structure store", cfg.Provider)
		}
		return ontologysqlite.New(structure), nil
	case "neo4j":
		if cfg.Neo4j.URI == "" {
			return nil, fmt.Errorf("ontology provider %q requires NASUTA_ONTOLOGY_NEO4J_URI", cfg.Provider)
		}
		return nil, fmt.Errorf("ontology provider %q is not available in this build", cfg.Provider)
	default:
		return nil, fmt.Errorf("ontology provider %q is unsupported", cfg.Provider)
	}
}
