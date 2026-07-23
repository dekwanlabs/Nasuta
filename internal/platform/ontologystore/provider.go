package ontologystore

import (
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	ontologysqlite "github.com/dekwanlabs/nasuta/internal/platform/ontologystore/sqlite"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// New is the only production dispatcher for ontology providers.
func New(cfg config.OntologyConfig, structure *store.SQLite) (ontology.Backend, error) {
	switch provider := strings.ToLower(cfg.Provider); provider {
	case "", "sqlite":
		return ontologysqlite.New(structure), nil
	case "neo4j":
		if cfg.Neo4j.URI == "" {
			return nil, fmt.Errorf("ontology provider %q requires ONTOLOGY_NEO4J_URI", provider)
		}
		return nil, fmt.Errorf("ontology provider %q is not available in this build", provider)
	default:
		return nil, fmt.Errorf("ontology provider %q is unsupported", provider)
	}
}
