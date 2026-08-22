package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	qdrantclient "github.com/qdrant/go-client/qdrant"
	"golang.org/x/sync/errgroup"
)

type Qdrant struct {
	client     *qdrantclient.Client
	collection string
}

// Keep the original sparse vector name so existing collections can be
// migrated in place without dropping dense service/document embeddings.
const codeSparseVector = "bm25"

const maxSemanticSearchLimit = 1000

func NewQdrant(ctx context.Context, cfg config.SemanticConfig) (*Qdrant, error) {
	host, port, endpointTLS, err := qdrantAddress(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := semanticTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	client, err := qdrantclient.NewClient(&qdrantclient.Config{
		Host:                   host,
		Port:                   port,
		APIKey:                 cfg.Auth.APIKey,
		UseTLS:                 cfg.TLS.Enabled || endpointTLS,
		TLSConfig:              tlsConfig,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant: create client: %w", err)
	}
	if _, err := client.ListCollections(ctx); err != nil {
		client.Close()
		return nil, fmt.Errorf("qdrant: connect %q: %w", cfg.Endpoint, err)
	}
	return &Qdrant{client: client, collection: cfg.Collection}, nil
}

func qdrantAddress(endpoint string) (string, int, bool, error) {
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Hostname() == "" {
			return "", 0, false, fmt.Errorf("qdrant: invalid endpoint %q", endpoint)
		}
		port := 6334
		if parsed.Port() != "" {
			port, err = strconv.Atoi(parsed.Port())
			if err != nil {
				return "", 0, false, fmt.Errorf("qdrant: invalid endpoint port %q", parsed.Port())
			}
		}
		return parsed.Hostname(), port, parsed.Scheme == "https", nil
	}
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", 0, false, fmt.Errorf("qdrant: endpoint must be host:port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, false, fmt.Errorf("qdrant: invalid endpoint port %q", portText)
	}
	return host, port, false, nil
}

func semanticTLSConfig(cfg config.SemanticTLS) (*tls.Config, error) {
	if !cfg.Enabled && cfg.CAFile == "" && cfg.ServerName == "" {
		return nil, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName}
	if cfg.CAFile == "" {
		return tlsConfig, nil
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("semantic TLS CA file %q: %w", cfg.CAFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("semantic TLS CA file %q contains no certificates", cfg.CAFile)
	}
	tlsConfig.RootCAs = pool
	return tlsConfig, nil
}

func (q *Qdrant) Capabilities() semantic.Capabilities { return semantic.RequiredCapabilities() }

func (q *Qdrant) Close() error { return q.client.Close() }

func (q *Qdrant) Ensure(ctx context.Context, schema semantic.Schema) error {
	if schema.Collection != "" && schema.Collection != q.collection {
		return fmt.Errorf("qdrant: schema collection %q does not match configured %q", schema.Collection, q.collection)
	}
	dim := schema.DenseDim
	if dim <= 0 {
		return fmt.Errorf("qdrant: dense dimension must be positive")
	}
	bm25Params := codeSparseVectorParams()
	exists, err := q.client.CollectionExists(ctx, q.collection)
	if err != nil {
		return fmt.Errorf("qdrant: check collection %q: %w", q.collection, err)
	}
	if exists {
		info, err := q.client.GetCollectionInfo(ctx, q.collection)
		if err != nil {
			return fmt.Errorf("qdrant: describe collection %q: %w", q.collection, err)
		}
		params := info.GetConfig().GetParams().GetVectorsConfig().GetParams()
		if params == nil {
			return fmt.Errorf("qdrant: collection %q does not use the compatible unnamed dense vector; rebuild into a new collection", q.collection)
		}
		if params.GetSize() != uint64(dim) || params.GetDistance() != qdrantclient.Distance_Cosine {
			return fmt.Errorf("qdrant: collection %q dense schema is size=%d distance=%s, want size=%d distance=Cosine; rebuild into a new collection", q.collection, params.GetSize(), params.GetDistance(), dim)
		}
		if err := q.client.UpdateCollection(ctx, &qdrantclient.UpdateCollection{
			CollectionName: q.collection,
			SparseVectorsConfig: &qdrantclient.SparseVectorConfig{
				Map: map[string]*qdrantclient.SparseVectorParams{
					codeSparseVector: bm25Params,
				},
			},
		}); err != nil {
			return fmt.Errorf("qdrant: ensure sparse vector on collection %q: %w", q.collection, err)
		}
		return nil
	}
	if err := q.client.CreateCollection(ctx, &qdrantclient.CreateCollection{
		CollectionName: q.collection,
		VectorsConfig: qdrantclient.NewVectorsConfig(&qdrantclient.VectorParams{
			Size:     uint64(dim),
			Distance: qdrantclient.Distance_Cosine,
		}),
		SparseVectorsConfig: qdrantclient.NewSparseVectorsConfig(map[string]*qdrantclient.SparseVectorParams{
			codeSparseVector: bm25Params,
		}),
	}); err != nil {
		return fmt.Errorf("qdrant: create collection %q: %w", q.collection, err)
	}
	return nil
}

func codeSparseVectorParams() *qdrantclient.SparseVectorParams {
	return &qdrantclient.SparseVectorParams{Modifier: qdrantclient.Modifier_Idf.Enum()}
}

func buildFilter(filters map[string]string) *qdrantclient.Filter {
	conds := make([]*qdrantclient.Condition, 0, len(filters))
	hasPath := false
	for k, v := range filters {
		if v == "" {
			continue
		}
		conds = append(conds, qdrantclient.NewMatch(k, v))
		if k == "kind" && v == "code_chunk" {
			hasPath = true
		}
	}
	f := &qdrantclient.Filter{Must: conds}
	if hasPath {
		f.MustNot = []*qdrantclient.Condition{qdrantclient.NewMatchText("path", platform.WorkspaceMetadataDir)}
	}
	if len(f.Must) == 0 && len(f.MustNot) == 0 {
		return nil
	}
	return f
}

func buildSemanticFilter(filter semantic.Filter) *qdrantclient.Filter {
	f := buildFilter(filter.Keywords)
	if f == nil {
		f = &qdrantclient.Filter{}
	}
	for field, values := range filter.AnyInteger {
		if len(values) > 0 {
			f.Must = append(f.Must, qdrantclient.NewMatchInts(field, values...))
		}
	}
	if len(f.Must) == 0 && len(f.MustNot) == 0 {
		return nil
	}
	return f
}

func pointsToHits(res []*qdrantclient.ScoredPoint, kind semantic.ScoreKind) []semantic.Hit {
	hits := make([]semantic.Hit, 0, len(res))
	for _, p := range res {
		hit := semantic.Hit{
			ID:        p.GetId().GetUuid(),
			Score:     p.GetScore(),
			ScoreKind: kind,
			Metadata:  payloadToMap(p.GetPayload()),
		}
		if kind == semantic.ScoreFusion {
			hit.FusionScore = hit.Score
		} else {
			hit.DenseScore = hit.Score
		}
		hits = append(hits, hit)
	}
	return hits
}

// groupsToHits flattens grouped query results into a flat hit list. With
// GroupSize=1 each group contributes its single best point; group order is
// best-first so the caller's top-N stays correct.
func groupsToHits(groups []*qdrantclient.PointGroup, kind semantic.ScoreKind) []semantic.Hit {
	out := make([]semantic.Hit, 0, len(groups))
	for _, g := range groups {
		for _, p := range g.GetHits() {
			hit := semantic.Hit{
				ID:        p.GetId().GetUuid(),
				Score:     p.GetScore(),
				ScoreKind: kind,
				Metadata:  payloadToMap(p.GetPayload()),
			}
			if kind == semantic.ScoreFusion {
				hit.FusionScore = hit.Score
			} else {
				hit.DenseScore = hit.Score
			}
			out = append(out, hit)
		}
	}
	return out
}

func (q *Qdrant) Search(ctx context.Context, query semantic.Query) ([]semantic.Hit, error) {
	if query.Limit <= 0 || query.Limit > maxSemanticSearchLimit || len(query.DenseVector) == 0 {
		return nil, fmt.Errorf("qdrant: search requires a dense vector and limit between 1 and %d", maxSemanticSearchLimit)
	}
	if query.SparseVector != nil {
		return q.searchHybrid(ctx, query)
	}
	return q.search(ctx, query.DenseVector, buildSemanticFilter(query.Filter), query.Limit, query.GroupBy)
}

func (q *Qdrant) search(ctx context.Context, vector []float32, filter *qdrantclient.Filter, limit int, groupKey string) ([]semantic.Hit, error) {
	fetchLimit := limit
	groupFallback := false
	if groupKey != "" {
		limU := uint64(limit)
		groupSize := uint64(1)
		groups, err := q.client.QueryGroups(ctx, &qdrantclient.QueryPointGroups{
			CollectionName: q.collection,
			Query:          qdrantclient.NewQuery(vector...),
			Filter:         filter,
			Limit:          &limU,
			GroupBy:        groupKey,
			GroupSize:      &groupSize,
			WithPayload:    qdrantclient.NewWithPayload(true),
		})
		if err != nil {
			log.WarnfCtx(ctx, "[qdrant] query groups (field=%q) failed, using bounded client grouping: %v", groupKey, err)
			fetchLimit = min(limit*6, 1000)
			groupFallback = true
		} else {
			return groupsToHits(groups, semantic.ScoreDense), nil
		}
	}
	limU := uint64(fetchLimit)
	res, err := q.client.Query(ctx, &qdrantclient.QueryPoints{
		CollectionName: q.collection,
		Query:          qdrantclient.NewQuery(vector...),
		Filter:         filter,
		Limit:          &limU,
		WithPayload:    qdrantclient.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant: search collection %q: %w", q.collection, err)
	}
	hits := pointsToHits(res, semantic.ScoreDense)
	if groupFallback {
		hits = deduplicateHits(hits, groupKey, limit)
	}
	return hits, nil
}

func (q *Qdrant) Count(ctx context.Context, filter semantic.Filter) (int, error) {
	exact := true
	n, err := q.client.Count(ctx, &qdrantclient.CountPoints{
		CollectionName: q.collection,
		Filter:         buildSemanticFilter(filter),
		Exact:          &exact,
	})
	if err != nil {
		return 0, fmt.Errorf("qdrant: count collection %q: %w", q.collection, err)
	}
	return int(n), nil
}

func (q *Qdrant) searchHybrid(ctx context.Context, query semantic.Query) ([]semantic.Hit, error) {
	sparse := query.SparseVector
	if len(sparse.Indices) == 0 || len(sparse.Indices) != len(sparse.Values) {
		return nil, fmt.Errorf("qdrant: invalid sparse vector")
	}
	filter := buildSemanticFilter(query.Filter)
	branchLimit := min(query.Limit*3, maxSemanticSearchLimit)
	var denseHits, sparseHits []semantic.Hit
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		denseHits, err = q.searchHybridBranch(groupCtx, qdrantclient.NewQueryDense(query.DenseVector), nil, filter, branchLimit, query.GroupBy, hybridDense)
		return err
	})
	group.Go(func() error {
		var err error
		sparseHits, err = q.searchHybridBranch(groupCtx, qdrantclient.NewQuerySparse(sparse.Indices, sparse.Values), stringPtr(codeSparseVector), filter, branchLimit, query.GroupBy, hybridSparse)
		return err
	})
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return fuseHybridHits(denseHits, sparseHits, query.Limit, query.GroupBy), nil
}

type hybridBranch uint8

const (
	hybridDense hybridBranch = iota
	hybridSparse
)

func (q *Qdrant) searchHybridBranch(
	ctx context.Context,
	vectorQuery *qdrantclient.Query,
	using *string,
	filter *qdrantclient.Filter,
	limit int,
	groupKey string,
	branch hybridBranch,
) ([]semantic.Hit, error) {
	fetchLimit := limit
	groupFallback := false
	if groupKey != "" {
		limU := uint64(limit)
		groupSize := uint64(1)
		groups, err := q.client.QueryGroups(ctx, &qdrantclient.QueryPointGroups{
			CollectionName: q.collection,
			Query:          vectorQuery,
			Using:          using,
			Filter:         filter,
			Limit:          &limU,
			GroupBy:        groupKey,
			GroupSize:      &groupSize,
			WithPayload:    qdrantclient.NewWithPayload(true),
		})
		if err != nil {
			log.WarnfCtx(ctx, "[qdrant] hybrid branch query groups (field=%q branch=%s) failed, using bounded client grouping: %v", groupKey, branch, err)
			fetchLimit = min(limit*6, maxSemanticSearchLimit)
			groupFallback = true
		} else {
			return groupsToBranchHits(groups, branch), nil
		}
	}
	limU := uint64(fetchLimit)
	res, err := q.client.Query(ctx, &qdrantclient.QueryPoints{
		CollectionName: q.collection,
		Query:          vectorQuery,
		Using:          using,
		Filter:         filter,
		Limit:          &limU,
		WithPayload:    qdrantclient.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant: %s branch search collection %q: %w", branch, q.collection, err)
	}
	hits := pointsToBranchHits(res, branch)
	if groupFallback {
		hits = deduplicateHits(hits, groupKey, limit)
	}
	return hits, nil
}

func pointsToBranchHits(res []*qdrantclient.ScoredPoint, branch hybridBranch) []semantic.Hit {
	hits := make([]semantic.Hit, 0, len(res))
	for _, point := range res {
		hits = append(hits, branchHit(point.GetId().GetUuid(), point.GetScore(), payloadToMap(point.GetPayload()), branch))
	}
	return hits
}

func groupsToBranchHits(groups []*qdrantclient.PointGroup, branch hybridBranch) []semantic.Hit {
	hits := make([]semantic.Hit, 0, len(groups))
	for _, group := range groups {
		for _, point := range group.GetHits() {
			hits = append(hits, branchHit(point.GetId().GetUuid(), point.GetScore(), payloadToMap(point.GetPayload()), branch))
		}
	}
	return hits
}

func branchHit(id string, score float32, metadata map[string]any, branch hybridBranch) semantic.Hit {
	hit := semantic.Hit{ID: id, Score: score, Metadata: metadata}
	if branch == hybridDense {
		hit.ScoreKind = semantic.ScoreDense
		hit.DenseScore = score
	} else {
		hit.SparseScore = score
	}
	return hit
}

const (
	hybridDenseWeight  = 0.75
	hybridSparseWeight = 0.25
	hybridRankOffset   = 10
)

func fuseHybridHits(denseHits, sparseHits []semantic.Hit, limit int, groupKey string) []semantic.Hit {
	if limit <= 0 {
		return nil
	}
	fused := make([]semantic.Hit, 0, len(denseHits)+len(sparseHits))
	byID := make(map[string]int, len(denseHits)+len(sparseHits))
	for rank, hit := range denseHits {
		if _, exists := byID[hit.ID]; exists {
			continue
		}
		hit.DenseRank = rank + 1
		hit.DenseScore = hit.Score
		hit.SparseScore = 0
		hit.SparseRank = 0
		hit.FusionScore = 0
		hit.ScoreKind = semantic.ScoreFusion
		byID[hit.ID] = len(fused)
		fused = append(fused, hit)
	}
	for rank, hit := range sparseHits {
		sparseRank := rank + 1
		if index, exists := byID[hit.ID]; exists {
			if fused[index].SparseRank == 0 {
				fused[index].SparseScore = hit.Score
				fused[index].SparseRank = sparseRank
			}
			continue
		}
		hit.DenseScore = 0
		hit.DenseRank = 0
		hit.SparseScore = hit.Score
		hit.SparseRank = sparseRank
		hit.FusionScore = 0
		hit.ScoreKind = semantic.ScoreFusion
		byID[hit.ID] = len(fused)
		fused = append(fused, hit)
	}
	for i := range fused {
		score := hybridDenseWeight*hybridRankScore(fused[i].DenseRank) +
			hybridSparseWeight*hybridRankScore(fused[i].SparseRank)
		fused[i].Score = score
		fused[i].FusionScore = score
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].FusionScore != fused[j].FusionScore {
			return fused[i].FusionScore > fused[j].FusionScore
		}
		if fused[i].DenseRank != fused[j].DenseRank {
			return rankBefore(fused[i].DenseRank, fused[j].DenseRank)
		}
		if fused[i].SparseRank != fused[j].SparseRank {
			return rankBefore(fused[i].SparseRank, fused[j].SparseRank)
		}
		return fused[i].ID < fused[j].ID
	})
	if groupKey != "" {
		return deduplicateHits(fused, groupKey, limit)
	}
	if len(fused) > limit {
		fused = fused[:limit]
	}
	return fused
}

func hybridRankScore(rank int) float32 {
	if rank <= 0 {
		return 0
	}
	return float32(hybridRankOffset+1) / float32(hybridRankOffset+rank)
}

func rankBefore(left, right int) bool {
	if left == 0 {
		return false
	}
	return right == 0 || left < right
}

func (branch hybridBranch) String() string {
	if branch == hybridSparse {
		return "sparse"
	}
	return "dense"
}

func deduplicateHits(hits []semantic.Hit, field string, limit int) []semantic.Hit {
	seen := make(map[string]struct{}, min(len(hits), limit))
	out := make([]semantic.Hit, 0, min(len(hits), limit))
	for _, hit := range hits {
		group := fmt.Sprint(hit.Metadata[field])
		if _, exists := seen[group]; exists {
			continue
		}
		seen[group] = struct{}{}
		out = append(out, hit)
		if len(out) == limit {
			break
		}
	}
	return out
}

func stringPtr(s string) *string { return &s }

// Upsert writes points into the collection.
func (q *Qdrant) Upsert(ctx context.Context, points []semantic.Record) error {
	if len(points) == 0 {
		return nil
	}
	qp := make([]*qdrantclient.PointStruct, 0, len(points))
	denseDim := len(points[0].DenseVector)
	for _, p := range points {
		if p.ID == "" || len(p.DenseVector) == 0 {
			return fmt.Errorf("qdrant: record requires ID and dense vector")
		}
		if p.SparseVector != nil && len(p.SparseVector.Indices) != len(p.SparseVector.Values) {
			return fmt.Errorf("qdrant: record %q has mismatched sparse vector indices and values", p.ID)
		}
		if len(p.DenseVector) != denseDim {
			return fmt.Errorf("qdrant: record %q dense dimension is %d, want %d", p.ID, len(p.DenseVector), denseDim)
		}
		ps := &qdrantclient.PointStruct{
			Id:      qdrantclient.NewID(p.ID),
			Payload: qdrantclient.NewValueMap(p.Metadata),
		}
		if p.SparseVector != nil && len(p.SparseVector.Indices) > 0 && len(p.SparseVector.Values) > 0 {
			ps.Vectors = qdrantclient.NewVectorsMap(map[string]*qdrantclient.Vector{
				"":               qdrantclient.NewVector(p.DenseVector...),
				codeSparseVector: qdrantclient.NewVectorSparse(p.SparseVector.Indices, p.SparseVector.Values),
			})
		} else {
			ps.Vectors = qdrantclient.NewVectors(p.DenseVector...)
		}
		qp = append(qp, ps)
	}
	_, err := q.client.Upsert(ctx, &qdrantclient.UpsertPoints{
		CollectionName: q.collection,
		Wait:           boolPtr(true),
		Points:         qp,
	})
	if err != nil {
		return fmt.Errorf("qdrant: upsert %d records: %w", len(points), err)
	}
	return nil
}

func (q *Qdrant) Delete(ctx context.Context, query semantic.DeleteQuery) error {
	if len(query.IDs) > 0 {
		return q.deletePoints(ctx, query.IDs)
	}
	filters := cloneKeywords(query.Filter.Keywords)
	if query.Repository != "" {
		filters["repo"] = query.Repository
	}
	if query.DocumentID != "" {
		filters["doc_id"] = query.DocumentID
	}
	query.Filter.Keywords = filters
	return q.deleteByFilterExcept(ctx, query.Filter, query.Except)
}

func cloneKeywords(source map[string]string) map[string]string {
	out := make(map[string]string, len(source)+2)
	for key, value := range source {
		out[key] = value
	}
	return out
}

// deletePoints removes a bounded set of points by ID.
func (q *Qdrant) deletePoints(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	pointIDs := make([]*qdrantclient.PointId, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			pointIDs = append(pointIDs, qdrantclient.NewID(id))
		}
	}
	if len(pointIDs) == 0 {
		return nil
	}
	_, err := q.client.Delete(ctx, &qdrantclient.DeletePoints{
		CollectionName: q.collection,
		Wait:           boolPtr(true),
		Points:         qdrantclient.NewPointsSelector(pointIDs...),
	})
	if err != nil {
		return fmt.Errorf("qdrant: delete %d points from %q: %w", len(pointIDs), q.collection, err)
	}
	return nil
}

// deleteByFilterExcept removes matching points except the active generation.
func (q *Qdrant) deleteByFilterExcept(ctx context.Context, filter, except semantic.Filter) error {
	must := make([]*qdrantclient.Condition, 0, len(filter.Keywords)+len(filter.AnyInteger))
	mustNot := make([]*qdrantclient.Condition, 0, len(except.Keywords)+len(except.AnyInteger))
	for key, value := range filter.Keywords {
		if value != "" {
			must = append(must, qdrantclient.NewMatch(key, value))
		}
	}
	for key, values := range filter.AnyInteger {
		if len(values) > 0 {
			must = append(must, qdrantclient.NewMatchInts(key, values...))
		}
	}
	for key, value := range except.Keywords {
		if value != "" {
			mustNot = append(mustNot, qdrantclient.NewMatch(key, value))
		}
	}
	for key, values := range except.AnyInteger {
		if len(values) > 0 {
			mustNot = append(mustNot, qdrantclient.NewMatchInts(key, values...))
		}
	}
	if len(must) == 0 {
		return fmt.Errorf("qdrant: refusing unbounded delete")
	}
	_, err := q.client.Delete(ctx, &qdrantclient.DeletePoints{
		CollectionName: q.collection,
		Wait:           boolPtr(true),
		Points: qdrantclient.NewPointsSelectorFilter(&qdrantclient.Filter{
			Must:    must,
			MustNot: mustNot,
		}),
	})
	if err != nil {
		return fmt.Errorf("qdrant: filtered delete from %q: %w", q.collection, err)
	}
	return nil
}

func boolPtr(value bool) *bool { return &value }

func payloadToMap(p map[string]*qdrantclient.Value) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		switch v.GetKind().(type) {
		case *qdrantclient.Value_IntegerValue:
			out[k] = v.GetIntegerValue()
		case *qdrantclient.Value_DoubleValue:
			out[k] = v.GetDoubleValue()
		case *qdrantclient.Value_BoolValue:
			out[k] = v.GetBoolValue()
		default:
			out[k] = v.GetStringValue()
		}
	}
	return out
}
