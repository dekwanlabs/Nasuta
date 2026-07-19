package milvus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/semantic"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	idField         = "id"
	denseField      = "dense_vector"
	sparseField     = "sparse_vector"
	metadataField   = "metadata"
	kindField       = "kind"
	repoField       = "repo"
	serviceField    = "service_name"
	documentIDField = "doc_id"
	pathField       = "path"
	langField       = "lang"
	statusField     = "status"
	generationField = "index_generation"
	userIDField     = "user_id"
	maxStringLength = 65535
	maxSearchLimit  = 1000
)

var filterFields = map[string]string{
	"kind": kindField, "repo": repoField, "service_name": serviceField,
	"doc_id": documentIDField, "path": pathField, "lang": langField, "status": statusField,
	"index_generation": generationField,
	"user_id":          userIDField,
}

// readConsistency forces Strong consistency so a search or query immediately
// after an upsert/delete observes the write. The SDK default (ClBounded) lags
// up to ~5s, which would make freshly indexed points invisible and break the
// indexing-then-query contract.
var readConsistency = []client.SearchQueryOptionFunc{
	client.WithSearchQueryConsistencyLevel(entity.ClStrong),
}

type Adapter struct {
	client     client.Client
	collection string
}

func New(ctx context.Context, cfg config.SemanticConfig) (*Adapter, error) {
	clientConfig := client.Config{
		Address: cfg.Endpoint, Username: cfg.Auth.Username, Password: cfg.Auth.Password,
		APIKey: cfg.Auth.APIKey, DBName: cfg.Namespace, EnableTLSAuth: cfg.TLS.Enabled,
	}
	if cfg.TLS.CAFile != "" || cfg.TLS.ServerName != "" {
		tlsConfig, err := tlsConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		clientConfig.EnableTLSAuth = false
		clientConfig.DialOptions = append(
			[]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))},
			client.DefaultGrpcOpts...,
		)
	}
	milvusClient, err := client.NewClient(ctx, clientConfig)
	if err != nil {
		return nil, fmt.Errorf("milvus: connect %q: %w", cfg.Endpoint, err)
	}
	state, err := milvusClient.CheckHealth(ctx)
	if err != nil {
		milvusClient.Close()
		return nil, fmt.Errorf("milvus: health check: %w", err)
	}
	if state == nil || !state.IsHealthy {
		milvusClient.Close()
		if state == nil {
			return nil, fmt.Errorf("milvus: health check returned no state")
		}
		return nil, fmt.Errorf("milvus: unhealthy: %s", strings.Join(state.Reasons, ", "))
	}
	return &Adapter{client: milvusClient, collection: cfg.Collection}, nil
}

func tlsConfig(cfg config.SemanticTLS) (*tls.Config, error) {
	result := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName}
	if cfg.CAFile == "" {
		return result, nil
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("milvus: TLS CA file %q: %w", cfg.CAFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("milvus: TLS CA file %q contains no certificates", cfg.CAFile)
	}
	result.RootCAs = pool
	return result, nil
}

func (*Adapter) Capabilities() semantic.Capabilities { return semantic.RequiredCapabilities() }

func (adapter *Adapter) Close() error { return adapter.client.Close() }

func (adapter *Adapter) Ensure(ctx context.Context, schema semantic.Schema) error {
	if schema.Collection != "" && schema.Collection != adapter.collection {
		return fmt.Errorf("milvus: schema collection %q does not match configured %q", schema.Collection, adapter.collection)
	}
	if schema.DenseDim <= 0 {
		return fmt.Errorf("milvus: dense dimension must be positive")
	}
	exists, err := adapter.client.HasCollection(ctx, adapter.collection)
	if err != nil {
		return fmt.Errorf("milvus: check collection %q: %w", adapter.collection, err)
	}
	if exists {
		if err := adapter.validateSchema(ctx, schema.DenseDim); err != nil {
			return err
		}
		return adapter.client.LoadCollection(ctx, adapter.collection, false)
	}
	collectionSchema := entity.NewSchema().WithName(adapter.collection).WithAutoID(false).
		WithField(entity.NewField().WithName(idField).WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithMaxLength(128)).
		WithField(entity.NewField().WithName(denseField).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(schema.DenseDim))).
		WithField(entity.NewField().WithName(sparseField).WithDataType(entity.FieldTypeSparseVector)).
		WithField(entity.NewField().WithName(kindField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(repoField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(serviceField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(documentIDField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(pathField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(langField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(statusField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(generationField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxStringLength)).
		WithField(entity.NewField().WithName(userIDField).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(metadataField).WithDataType(entity.FieldTypeJSON))
	if err := adapter.client.CreateCollection(ctx, collectionSchema, entity.DefaultShardNumber); err != nil {
		return fmt.Errorf("milvus: create collection %q: %w", adapter.collection, err)
	}
	denseIndex, _ := entity.NewIndexFlat(entity.COSINE)
	if err := adapter.client.CreateIndex(ctx, adapter.collection, denseField, denseIndex, false); err != nil {
		return fmt.Errorf("milvus: create dense index: %w", err)
	}
	sparseIndex, _ := entity.NewIndexSparseInverted(entity.IP, 0)
	if err := adapter.client.CreateIndex(ctx, adapter.collection, sparseField, sparseIndex, false); err != nil {
		return fmt.Errorf("milvus: create sparse index: %w", err)
	}
	if err := adapter.client.LoadCollection(ctx, adapter.collection, false); err != nil {
		return fmt.Errorf("milvus: load collection %q: %w", adapter.collection, err)
	}
	return nil
}

func (adapter *Adapter) validateSchema(ctx context.Context, denseDim int) error {
	collection, err := adapter.client.DescribeCollection(ctx, adapter.collection)
	if err != nil {
		return fmt.Errorf("milvus: describe collection %q: %w", adapter.collection, err)
	}
	if collection.Schema == nil {
		return fmt.Errorf("milvus: collection %q returned no schema", adapter.collection)
	}
	if collection.Schema.AutoID {
		return fmt.Errorf("milvus: collection %q uses auto IDs; rebuild into a new collection", adapter.collection)
	}
	required := map[string]entity.FieldType{
		idField: entity.FieldTypeVarChar, denseField: entity.FieldTypeFloatVector,
		sparseField: entity.FieldTypeSparseVector, metadataField: entity.FieldTypeJSON,
		kindField: entity.FieldTypeVarChar, repoField: entity.FieldTypeVarChar,
		serviceField: entity.FieldTypeVarChar, documentIDField: entity.FieldTypeVarChar,
		pathField: entity.FieldTypeVarChar, langField: entity.FieldTypeVarChar,
		statusField: entity.FieldTypeVarChar, generationField: entity.FieldTypeVarChar,
		userIDField: entity.FieldTypeInt64,
	}
	for _, field := range collection.Schema.Fields {
		expected, ok := required[field.Name]
		if !ok {
			continue
		}
		if field.DataType != expected {
			return fmt.Errorf("milvus: collection %q schema field %q is %v, want %v; rebuild into a new collection", adapter.collection, field.Name, field.DataType, expected)
		}
		if field.Name == idField && (!field.PrimaryKey || field.AutoID) {
			return fmt.Errorf("milvus: collection %q field %q must be a caller-assigned primary key; rebuild into a new collection", adapter.collection, idField)
		}
		if field.Name == denseField && field.TypeParams[entity.TypeParamDim] != strconv.Itoa(denseDim) {
			return fmt.Errorf("milvus: collection %q dense dimension is %q, want %d; rebuild into a new collection", adapter.collection, field.TypeParams[entity.TypeParamDim], denseDim)
		}
		delete(required, field.Name)
	}
	if len(required) > 0 {
		missing := make([]string, 0, len(required))
		for field := range required {
			missing = append(missing, field)
		}
		sort.Strings(missing)
		return fmt.Errorf("milvus: collection %q schema is missing %s; rebuild into a new collection", adapter.collection, strings.Join(missing, ", "))
	}
	return nil
}

func (adapter *Adapter) Search(ctx context.Context, query semantic.Query) ([]semantic.Hit, error) {
	if query.Limit <= 0 || query.Limit > maxSearchLimit || len(query.DenseVector) == 0 {
		return nil, fmt.Errorf("milvus: search requires a dense vector and limit between 1 and %d", maxSearchLimit)
	}
	expr, err := compileFilter(query.Filter)
	if err != nil {
		return nil, err
	}
	fetchLimit := query.Limit
	if query.GroupBy != "" {
		if _, ok := filterFields[query.GroupBy]; !ok {
			return nil, fmt.Errorf("milvus: unsupported group field %q", query.GroupBy)
		}
		fetchLimit = min(query.Limit*6, 1000)
	}
	var results []client.SearchResult
	if query.SparseVector == nil {
		params, _ := entity.NewIndexFlatSearchParam()
		results, err = adapter.client.Search(ctx, adapter.collection, nil, expr, []string{metadataField},
			[]entity.Vector{entity.FloatVector(query.DenseVector)}, denseField, entity.COSINE, fetchLimit, params, readConsistency...)
	} else {
		if len(query.SparseVector.Indices) == 0 || len(query.SparseVector.Indices) != len(query.SparseVector.Values) {
			return nil, fmt.Errorf("milvus: invalid sparse vector")
		}
		indices := append([]uint32(nil), query.SparseVector.Indices...)
		values := append([]float32(nil), query.SparseVector.Values...)
		sparseVector, sparseErr := entity.NewSliceSparseEmbedding(indices, values)
		if sparseErr != nil {
			return nil, fmt.Errorf("milvus: sparse vector: %w", sparseErr)
		}
		denseParams, _ := entity.NewIndexFlatSearchParam()
		sparseParams, _ := entity.NewIndexSparseInvertedSearchParam(0)
		results, err = adapter.client.HybridSearch(ctx, adapter.collection, nil, fetchLimit, []string{metadataField},
			client.NewRRFReranker(), []*client.ANNSearchRequest{
				client.NewANNSearchRequest(denseField, entity.COSINE, expr, []entity.Vector{entity.FloatVector(query.DenseVector)}, denseParams, fetchLimit),
				client.NewANNSearchRequest(sparseField, entity.IP, expr, []entity.Vector{sparseVector}, sparseParams, fetchLimit),
			}, readConsistency...)
	}
	if err != nil {
		return nil, fmt.Errorf("milvus: search collection %q: %w", adapter.collection, err)
	}
	kind := semantic.ScoreDense
	if query.SparseVector != nil {
		kind = semantic.ScoreFusion
	}
	hits, err := decodeHits(results, kind)
	if err != nil {
		return nil, err
	}
	if query.GroupBy != "" {
		hits = deduplicateGroups(hits, query.GroupBy, query.Limit)
	} else if len(hits) > query.Limit {
		hits = hits[:query.Limit]
	}
	return hits, nil
}

func decodeHits(results []client.SearchResult, kind semantic.ScoreKind) ([]semantic.Hit, error) {
	if len(results) == 0 {
		return nil, nil
	}
	result := results[0]
	if result.Err != nil {
		return nil, fmt.Errorf("milvus: decode search result: %w", result.Err)
	}
	metadataColumn := result.Fields.GetColumn(metadataField)
	if metadataColumn == nil {
		return nil, fmt.Errorf("milvus: search result omitted metadata")
	}
	hits := make([]semantic.Hit, 0, result.ResultCount)
	for i := 0; i < result.ResultCount; i++ {
		id, err := result.IDs.GetAsString(i)
		if err != nil {
			return nil, fmt.Errorf("milvus: decode hit ID: %w", err)
		}
		raw, err := metadataColumn.Get(i)
		if err != nil {
			return nil, fmt.Errorf("milvus: decode hit metadata: %w", err)
		}
		data, ok := raw.([]byte)
		if !ok {
			return nil, fmt.Errorf("milvus: metadata has unexpected type %T", raw)
		}
		metadata := map[string]any{}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, fmt.Errorf("milvus: decode hit metadata JSON: %w", err)
		}
		hit := semantic.Hit{ID: id, Score: result.Scores[i], ScoreKind: kind, Metadata: metadata}
		if kind == semantic.ScoreFusion {
			hit.FusionScore = hit.Score
		} else {
			hit.DenseScore = hit.Score
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

func deduplicateGroups(hits []semantic.Hit, field string, limit int) []semantic.Hit {
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

func (adapter *Adapter) Upsert(ctx context.Context, records []semantic.Record) error {
	if len(records) == 0 {
		return nil
	}
	ids := make([]string, 0, len(records))
	dense := make([][]float32, 0, len(records))
	sparse := make([]entity.SparseEmbedding, 0, len(records))
	metadata := make([][]byte, 0, len(records))
	stringsByField := map[string][]string{
		kindField: {}, repoField: {}, serviceField: {}, documentIDField: {}, pathField: {}, langField: {}, statusField: {}, generationField: {},
	}
	userIDs := make([]int64, 0, len(records))
	denseDim := len(records[0].DenseVector)
	for _, record := range records {
		if record.ID == "" || len(record.DenseVector) == 0 {
			return fmt.Errorf("milvus: record requires ID and dense vector")
		}
		if len(record.ID) > 128 {
			return fmt.Errorf("milvus: record ID %q exceeds 128 bytes", record.ID)
		}
		if len(record.DenseVector) != denseDim {
			return fmt.Errorf("milvus: record %q dense dimension is %d, want %d", record.ID, len(record.DenseVector), denseDim)
		}
		sparseVector := record.SparseVector
		if sparseVector == nil {
			sparseVector = &semantic.SparseVector{}
		}
		indices := append([]uint32(nil), sparseVector.Indices...)
		values := append([]float32(nil), sparseVector.Values...)
		embedding, err := entity.NewSliceSparseEmbedding(indices, values)
		if err != nil {
			return fmt.Errorf("milvus: record %q sparse vector: %w", record.ID, err)
		}
		encoded, err := json.Marshal(record.Metadata)
		if err != nil {
			return fmt.Errorf("milvus: record %q metadata: %w", record.ID, err)
		}
		ids = append(ids, record.ID)
		dense = append(dense, record.DenseVector)
		sparse = append(sparse, embedding)
		metadata = append(metadata, encoded)
		for field := range stringsByField {
			value := metadataString(record.Metadata[field])
			if len(value) > maxStringLength {
				return fmt.Errorf("milvus: record %q metadata field %q exceeds %d bytes", record.ID, field, maxStringLength)
			}
			stringsByField[field] = append(stringsByField[field], value)
		}
		userIDs = append(userIDs, metadataInt64(record.Metadata[userIDField]))
	}
	_, err := adapter.client.Upsert(ctx, adapter.collection, "",
		entity.NewColumnVarChar(idField, ids),
		entity.NewColumnFloatVector(denseField, denseDim, dense),
		entity.NewColumnSparseVectors(sparseField, sparse),
		entity.NewColumnVarChar(kindField, stringsByField[kindField]),
		entity.NewColumnVarChar(repoField, stringsByField[repoField]),
		entity.NewColumnVarChar(serviceField, stringsByField[serviceField]),
		entity.NewColumnVarChar(documentIDField, stringsByField[documentIDField]),
		entity.NewColumnVarChar(pathField, stringsByField[pathField]),
		entity.NewColumnVarChar(langField, stringsByField[langField]),
		entity.NewColumnVarChar(statusField, stringsByField[statusField]),
		entity.NewColumnVarChar(generationField, stringsByField[generationField]),
		entity.NewColumnInt64(userIDField, userIDs),
		entity.NewColumnJSONBytes(metadataField, metadata),
	)
	if err != nil {
		return fmt.Errorf("milvus: upsert %d records: %w", len(records), err)
	}
	return nil
}

func metadataString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func metadataInt64(value any) int64 {
	switch number := value.(type) {
	case int64:
		return number
	case int:
		return int64(number)
	case float64:
		return int64(number)
	case json.Number:
		result, _ := number.Int64()
		return result
	default:
		return 0
	}
}

func (adapter *Adapter) Delete(ctx context.Context, query semantic.DeleteQuery) error {
	expr, err := compileDelete(query)
	if err != nil {
		return err
	}
	if err := adapter.client.Delete(ctx, adapter.collection, "", expr); err != nil {
		return fmt.Errorf("milvus: delete from %q: %w", adapter.collection, err)
	}
	return nil
}

func compileDelete(query semantic.DeleteQuery) (string, error) {
	if len(query.IDs) > 0 {
		values := make([]string, 0, len(query.IDs))
		for _, id := range query.IDs {
			if id != "" {
				values = append(values, strconv.Quote(id))
			}
		}
		if len(values) == 0 {
			return "", fmt.Errorf("milvus: delete has no valid IDs")
		}
		return idField + " in [" + strings.Join(values, ",") + "]", nil
	}
	filter := query.Filter
	filter.Keywords = cloneKeywords(filter.Keywords)
	if query.Repository != "" {
		filter.Keywords[repoField] = query.Repository
	}
	if query.DocumentID != "" {
		filter.Keywords[documentIDField] = query.DocumentID
	}
	include, err := compileFilter(filter)
	if err != nil {
		return "", err
	}
	if include == "" {
		return "", fmt.Errorf("milvus: refusing unbounded delete")
	}
	exclude, err := compileFilter(query.Except)
	if err != nil {
		return "", err
	}
	if exclude != "" {
		include += " && !(" + exclude + ")"
	}
	return include, nil
}

func cloneKeywords(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+2)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func compileFilter(filter semantic.Filter) (string, error) {
	parts := make([]string, 0, len(filter.Keywords)+len(filter.AnyInteger))
	keys := make([]string, 0, len(filter.Keywords))
	for key := range filter.Keywords {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		field, ok := filterFields[key]
		if !ok || field == userIDField {
			return "", fmt.Errorf("milvus: unsupported keyword filter %q", key)
		}
		if value := filter.Keywords[key]; value != "" {
			parts = append(parts, field+" == "+strconv.Quote(value))
		}
	}
	keys = keys[:0]
	for key := range filter.AnyInteger {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		field, ok := filterFields[key]
		if !ok || field != userIDField {
			return "", fmt.Errorf("milvus: unsupported integer filter %q", key)
		}
		values := filter.AnyInteger[key]
		if len(values) == 0 {
			continue
		}
		numbers := make([]string, len(values))
		for i, value := range values {
			numbers[i] = strconv.FormatInt(value, 10)
		}
		parts = append(parts, field+" in ["+strings.Join(numbers, ",")+"]")
	}
	return strings.Join(parts, " && "), nil
}

func (adapter *Adapter) Count(ctx context.Context, filter semantic.Filter) (int, error) {
	expr, err := compileFilter(filter)
	if err != nil {
		return 0, err
	}
	if expr == "" {
		stats, err := adapter.client.GetCollectionStatistics(ctx, adapter.collection)
		if err != nil {
			return 0, fmt.Errorf("milvus: collection statistics: %w", err)
		}
		count, err := strconv.Atoi(stats["row_count"])
		if err != nil {
			return 0, fmt.Errorf("milvus: invalid row_count %q: %w", stats["row_count"], err)
		}
		return count, nil
	}
	result, err := adapter.client.Query(ctx, adapter.collection, nil, expr, []string{"count(*)"}, readConsistency...)
	if err != nil {
		return 0, fmt.Errorf("milvus: filtered count: %w", err)
	}
	column := result.GetColumn("count(*)")
	if column == nil || column.Len() == 0 {
		return 0, fmt.Errorf("milvus: filtered count result is empty")
	}
	count, err := column.GetAsInt64(0)
	if err != nil {
		return 0, fmt.Errorf("milvus: decode filtered count: %w", err)
	}
	return int(count), nil
}
