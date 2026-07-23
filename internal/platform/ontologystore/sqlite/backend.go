package sqlite

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

type Backend struct{ store *store.SQLite }

func New(structure *store.SQLite) *Backend { return &Backend{store: structure} }

func (backend *Backend) Resolve(ctx context.Context, query ontology.ResolveQuery) ([]ontology.Entity, error) {
	return backend.store.ResolveOntology(ctx, query)
}

func (backend *Backend) EntitiesByID(ctx context.Context, query ontology.EntityQuery) ([]ontology.Entity, error) {
	return backend.store.OntologyEntitiesByID(ctx, query)
}

func (backend *Backend) Neighbors(ctx context.Context, query ontology.NeighborQuery) ([]ontology.Fact, bool, error) {
	return backend.store.OntologyNeighbors(ctx, query)
}

func (backend *Backend) FindPaths(ctx context.Context, query ontology.PathQuery) ([]ontology.Path, bool, error) {
	return ontology.FindBoundedPaths(ctx, backend, query)
}

func (backend *Backend) Stats(ctx context.Context) (ontology.Stats, error) {
	return backend.store.OntologyStats(ctx)
}

func (backend *Backend) PublishWorkspace(ctx context.Context, snapshot ontology.WorkspaceSnapshot) error {
	expected := ontology.GenerationFor(snapshot.Structure.Repositories)
	if snapshot.Generation != expected {
		return fmt.Errorf("workspace generation %q does not match structure %q", snapshot.Generation, expected)
	}
	return backend.store.ReplaceWorkspace(ctx, snapshot.Generation, snapshot.Structure, snapshot.Ontology)
}

// The structure store owns the shared connection lifecycle.
func (backend *Backend) Close() error { return nil }

var _ ontology.Backend = (*Backend)(nil)
