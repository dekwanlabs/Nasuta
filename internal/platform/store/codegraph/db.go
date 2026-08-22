package codegraph

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/dekwanlabs/nasuta/log"

	_ "modernc.org/sqlite"
)

// DB wraps the codegraph SQLite database for function-level call chain queries.
type DB struct {
	mu     sync.RWMutex
	db     *sql.DB
	dbPath string
}

// Node is a single symbol in the codegraph.
type Node struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	QualifiedName string  `json:"qualifiedName"`
	FilePath      string  `json:"filePath"`
	Language      string  `json:"language"`
	StartLine     int     `json:"startLine"`
	EndLine       int     `json:"endLine"`
	Signature     string  `json:"signature,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"` // call-edge parse confidence from codegraph metadata
}

// Edge is a relationship between two nodes.
type Edge struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Kind       string  `json:"kind"`
	Line       int     `json:"line,omitempty"`
	Col        int     `json:"col,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Provenance string  `json:"provenance,omitempty"`
}

// CallHop preserves the concrete call site between two symbols.
type CallHop struct {
	Source Node `json:"source"`
	Target Node `json:"target"`
	Edge   Edge `json:"edge"`
	Depth  int  `json:"depth"`
}

// SymbolQuery is one bounded full-text symbol lookup.
type SymbolQuery struct {
	Terms        []string
	Kinds        []string
	PathPrefixes []string
	Limit        int
}

// Open opens the codegraph SQLite database for read-only use.
// It returns nil with no error when the database file does not exist.
// Callers can therefore degrade gracefully.
func Open(workspaceRoot string) (*DB, error) {
	dbPath := filepath.Join(workspaceRoot, ".codegraph", "codegraph.db")
	d, err := openDatabase(dbPath)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, nil
	}
	return &DB{db: d, dbPath: dbPath}, nil
}

func openDatabase(dbPath string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("codegraph: stat %s: %w", dbPath, err)
	}
	d, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("codegraph: open %s: %w", dbPath, err)
	}
	d.SetMaxOpenConns(4)
	if !schemaOK(d) {
		_ = d.Close()
		return nil, nil // file exists but tables missing — treat as not indexed
	}
	if err := validateNodePaths(d); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// schemaOK reports whether the expected tables exist, so callers don't get
// "no such table" errors on a file that exists but was never initialised.
const schemaCheckQuery = `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('nodes','edges')`

func schemaOK(d *sql.DB) bool {
	var n int
	if err := d.QueryRow(schemaCheckQuery).Scan(&n); err != nil || n != 2 {
		return false
	}
	return true
}

func validateNodePaths(d *sql.DB) error {
	var invalid int
	if err := d.QueryRow(`SELECT COUNT(*) FROM nodes WHERE file_path NOT LIKE 'repos/%'`).Scan(&invalid); err != nil {
		return fmt.Errorf("codegraph: validate node paths: %w", err)
	}
	if invalid > 0 {
		return fmt.Errorf("codegraph: index contains %d nodes outside canonical repos/ paths; run a full rebuild", invalid)
	}
	return nil
}

// Refresh switches future queries to the database produced by a full rebuild.
func (d *DB) Refresh() error {
	next, err := openDatabase(d.dbPath)
	if err != nil {
		return err
	}
	if next == nil {
		return fmt.Errorf("codegraph: refresh %s: database unavailable", d.dbPath)
	}

	d.mu.Lock()
	previous := d.db
	d.db = next
	d.mu.Unlock()

	if previous != nil {
		if err := previous.Close(); err != nil {
			log.Warnf("[codegraph] close replaced database: %v", err)
		}
	}
	return nil
}

// Close closes the database.
func (d *DB) Close() error {
	d.mu.Lock()
	current := d.db
	d.db = nil
	d.mu.Unlock()
	if current == nil {
		return nil
	}
	return current.Close()
}

func (d *DB) query(query string, args ...any) (*sql.Rows, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.db == nil {
		return nil, sql.ErrConnDone
	}
	return d.db.Query(query, args...)
}

func (d *DB) scanRow(query string, args []any, dest ...any) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.db == nil {
		return sql.ErrConnDone
	}
	return d.db.QueryRow(query, args...).Scan(dest...)
}

func (d *DB) callEdges(nodeID, direction string, limit int) ([]CallHop, bool, error) {
	where := "e.source = ?"
	if direction == "callers" {
		where = "e.target = ?"
	}
	rows, err := d.query(`
		SELECT
			s.id,s.name,s.kind,s.qualified_name,s.file_path,s.language,s.start_line,s.end_line,COALESCE(s.signature,''),
			t.id,t.name,t.kind,t.qualified_name,t.file_path,t.language,t.start_line,t.end_line,COALESCE(t.signature,''),
			e.kind,COALESCE(e.line,0),COALESCE(e.col,0),COALESCE(e.provenance,''),COALESCE(e.metadata,'')
		FROM edges e
		JOIN nodes s ON s.id=e.source
		JOIN nodes t ON t.id=e.target
		WHERE e.kind='calls' AND `+where+`
		ORDER BY e.line,e.col,s.file_path,s.start_line,t.file_path,t.start_line
		LIMIT ?`, nodeID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	hops := make([]CallHop, 0, limit)
	for rows.Next() {
		var hop CallHop
		var metadata string
		if err := rows.Scan(
			&hop.Source.ID, &hop.Source.Name, &hop.Source.Kind, &hop.Source.QualifiedName,
			&hop.Source.FilePath, &hop.Source.Language, &hop.Source.StartLine, &hop.Source.EndLine, &hop.Source.Signature,
			&hop.Target.ID, &hop.Target.Name, &hop.Target.Kind, &hop.Target.QualifiedName,
			&hop.Target.FilePath, &hop.Target.Language, &hop.Target.StartLine, &hop.Target.EndLine, &hop.Target.Signature,
			&hop.Edge.Kind, &hop.Edge.Line, &hop.Edge.Col, &hop.Edge.Provenance, &metadata,
		); err != nil {
			return nil, false, err
		}
		hop.Edge.Source = hop.Source.ID
		hop.Edge.Target = hop.Target.ID
		hop.Edge.Confidence = parseConfidence(metadata)
		hop.Source.Confidence = hop.Edge.Confidence
		hop.Target.Confidence = hop.Edge.Confidence
		hops = append(hops, hop)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(hops) > limit
	if more {
		hops = hops[:limit]
	}
	return hops, more, nil
}

// CallEdges returns one bounded adjacency page and whether more edges exist.
func (d *DB) CallEdges(nodeID, direction string, limit int) ([]CallHop, bool, error) {
	if direction != "callers" && direction != "callees" {
		return nil, false, fmt.Errorf("codegraph: invalid call direction %q", direction)
	}
	if limit <= 0 {
		limit = 20
	}
	return d.callEdges(nodeID, direction, limit)
}

// parseConfidence extracts the "confidence" float from a codegraph edge metadata JSON blob.
func parseConfidence(metadata string) float64 {
	if metadata == "" {
		return 0
	}
	var m struct {
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return 0
	}
	return m.Confidence
}

// FindNodeByFile finds a symbol by file path and line number.
// It prefers callable nodes over broader class or file matches.
// Exact file_path match is fast; non-canonical paths fall back to a slower suffix LIKE.
func (d *DB) FindNodeByFile(filePath string, line int) (*Node, error) {
	log.Infof("[codegraph] FindNodeByFile: file=%s line=%d", filePath, line)
	if n, err := d.findNodeByFile(filePath, line, "file_path = ?", filePath); err == nil {
		log.Infof("[codegraph] FindNodeByFile: found kind=%s name=%s id=%s (exact)", n.Kind, n.Name, n.ID)
		return n, nil
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	n, err := d.findNodeByFile(filePath, line, "file_path LIKE ?", "%"+filePath)
	if err != nil {
		log.Infof("[codegraph] FindNodeByFile: not found: %v", err)
		return nil, err
	}
	log.Infof("[codegraph] FindNodeByFile: found kind=%s name=%s id=%s (suffix)", n.Kind, n.Name, n.ID)
	return n, nil
}

// findNodeByFile runs the ranked node lookup with the given file_path predicate.
func (d *DB) findNodeByFile(filePath string, line int, pathPred, pathArg string) (*Node, error) {
	n := &Node{}
	err := d.scanRow(`
		SELECT id, name, kind, qualified_name, file_path, language, start_line, end_line, COALESCE(signature,'')
		FROM nodes WHERE `+pathPred+` AND start_line <= ? AND end_line >= ?
		ORDER BY
			CASE kind WHEN 'method' THEN 0 WHEN 'function' THEN 0 WHEN 'route' THEN 1 ELSE 2 END,
			(end_line - start_line) ASC
		LIMIT 1`,
		[]any{pathArg, line, line},
		&n.ID, &n.Name, &n.Kind, &n.QualifiedName, &n.FilePath, &n.Language, &n.StartLine, &n.EndLine, &n.Signature)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// FindFilesByName returns file paths whose base name matches name (LIKE match).
// Used to resolve short LLM citations (e.g. "CustomerEventCollector.java:55")
// to a full workspace path.
func (d *DB) FindFilesByName(name string, limit int) ([]string, error) {
	rows, err := d.query(
		`SELECT DISTINCT file_path FROM nodes WHERE file_path LIKE ? LIMIT ?`,
		"%"+name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			files = append(files, p)
		}
	}
	return files, rows.Err()
}

// RouteAt finds the HTTP route declared at a file+line (e.g. "GET" "/room/device/list").
func (d *DB) RouteAt(filePath string, line int) (httpMethod, path string, ok bool) {
	var name string
	err := d.scanRow(`
		SELECT name FROM nodes
		WHERE kind='route' AND file_path LIKE ? AND start_line = ?
		LIMIT 1`, []any{"%" + filePath, line}, &name)
	if err != nil {
		return "", "", false
	}
	return parseRouteName(name)
}

// RouteForNode finds the closest route annotation owned by one callable symbol.
func (d *DB) RouteForNode(node Node) (httpMethod, path string, ok bool) {
	start := max(node.StartLine-8, 1)
	var name string
	err := d.scanRow(`SELECT r.name FROM nodes r JOIN nodes m ON m.id=? AND m.file_path=r.file_path
WHERE r.kind='route' AND r.file_path=? AND r.start_line BETWEEN ? AND ?
AND ((m.start_line<=r.start_line AND m.end_line>=r.start_line) OR
     (m.start_line>r.start_line AND m.start_line<=r.start_line+8))
ORDER BY abs(r.start_line-?) LIMIT 1`, []any{node.ID, node.FilePath, start, node.EndLine, node.StartLine}, &name)
	if err != nil {
		return "", "", false
	}
	return parseRouteName(name)
}

// HTTPClientRouteAt extracts a literal outbound HTTP route from a client call site.
// It intentionally reports only routes that are statically provable from source;
// configured or computed URLs remain unresolved rather than being guessed.
func (d *DB) HTTPClientRouteAt(filePath string, line int) (httpMethod, path string, ok bool) {
	text, err := d.readWorkspaceFile(filePath)
	if err != nil || text == "" {
		return "", "", false
	}
	lines := strings.Split(text, "\n")
	if line < 1 || line > len(lines) {
		return "", "", false
	}
	start := max(line-4, 1)
	end := min(line+4, len(lines))
	window := strings.Join(lines[start-1:end], "\n")

	urlRE := regexp.MustCompile(`(?i)(?:https?://[^"'\s)]+|\.uri\s*\(\s*["']([^"']+)["'])`)
	matches := urlRE.FindAllStringSubmatch(window, -1)
	if len(matches) == 0 {
		return "", "", false
	}
	for _, match := range matches {
		candidate := match[0]
		if match[1] != "" {
			candidate = match[1]
		} else {
			candidate = strings.Trim(candidate, `"'`)
		}
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") {
			continue
		}
		method := httpMethodForClientWindow(window)
		if method == "" {
			continue
		}
		return method, parsed.Path, true
	}
	return "", "", false
}

func (d *DB) readWorkspaceFile(filePath string) (string, error) {
	path := filePath
	if !filepath.IsAbs(path) {
		workspace := filepath.Dir(filepath.Dir(d.dbPath))
		path = filepath.Join(workspace, filepath.FromSlash(filePath))
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

func httpMethodForClientWindow(window string) string {
	lower := strings.ToLower(window)
	switch {
	case strings.Contains(lower, "postforobject"), strings.Contains(lower, "postforentity"),
		strings.Contains(lower, "postforlocation"), strings.Contains(lower, "httppost"),
		strings.Contains(lower, ".post()"), strings.Contains(lower, "httpmethod.post"):
		return "POST"
	case strings.Contains(lower, "putforobject"), strings.Contains(lower, "putforentity"),
		strings.Contains(lower, "httpput"), strings.Contains(lower, ".put()"),
		strings.Contains(lower, "httpmethod.put"):
		return "PUT"
	case strings.Contains(lower, "delete"), strings.Contains(lower, "httpdelete"),
		strings.Contains(lower, ".delete()"), strings.Contains(lower, "httpmethod.delete"):
		return "DELETE"
	case strings.Contains(lower, "patch"), strings.Contains(lower, "httppatch"),
		strings.Contains(lower, ".patch()"), strings.Contains(lower, "httpmethod.patch"):
		return "PATCH"
	case strings.Contains(lower, "getforobject"), strings.Contains(lower, "getforentity"),
		strings.Contains(lower, "httpget"), strings.Contains(lower, ".get()"),
		strings.Contains(lower, "httpmethod.get"):
		return "GET"
	default:
		return ""
	}
}

func parseRouteName(name string) (httpMethod, path string, ok bool) {
	parts := strings.SplitN(name, " ", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ResolveRouteMethodInFile finds the client method for one route in an exact file.
func (d *DB) ResolveRouteMethodInFile(filePath, httpMethod, path string) (*Node, error) {
	var line int
	err := d.scanRow(`SELECT start_line FROM nodes
WHERE kind='route' AND file_path=? AND (name=? OR name LIKE ?)
ORDER BY CASE WHEN name=? THEN 0 ELSE 1 END,length(name) ASC LIMIT 1`,
		[]any{filePath, httpMethod + " " + path, httpMethod + " %" + path, httpMethod + " " + path}, &line)
	if err != nil {
		return nil, err
	}
	return d.findMethodForRoute(filePath, line)
}

// ResolveDownstreamMethod finds the implementing controller method in the target
// service for a Feign call (matched by HTTP method + path suffix).
func (d *DB) ResolveDownstreamMethod(targetService, httpMethod, path string) (*Node, error) {
	return d.resolveDownstreamMethod("file_path LIKE ?", []any{"%" + targetService + "%"}, httpMethod, path)
}

// ResolveDownstreamMethodInPath restricts route resolution to one canonical module prefix.
func (d *DB) ResolveDownstreamMethodInPath(pathPrefix, httpMethod, path string) (*Node, error) {
	return d.ResolveRouteMethodInPath(pathPrefix, httpMethod, path)
}

// ResolveRouteMethodInPath resolves a route implementation anywhere below one
// normalized repository or module prefix. It is used for runtime applications
// whose HTTP controllers and Feign clients live in sibling Maven modules.
func (d *DB) ResolveRouteMethodInPath(pathPrefix, httpMethod, path string) (*Node, error) {
	pathPrefix = strings.Trim(strings.ReplaceAll(pathPrefix, "\\", "/"), "/")
	return d.resolveDownstreamMethod("(file_path=? OR (file_path>=? AND file_path<?))", []any{pathPrefix, pathPrefix + "/", pathPrefix + "0"}, httpMethod, path)
}

func (d *DB) resolveDownstreamMethod(pathPredicate string, pathArgs []any, httpMethod, path string) (*Node, error) {
	var file string
	var line int
	args := make([]any, 0, len(pathArgs)+3)
	args = append(args, pathArgs...)
	args = append(args, httpMethod+" "+path, httpMethod+" %"+path, httpMethod+" "+path)
	err := d.scanRow(`
		SELECT file_path, start_line FROM nodes
		WHERE kind='route' AND `+pathPredicate+`
		  AND (name = ? OR name LIKE ?)
		ORDER BY CASE WHEN name = ? THEN 0 ELSE 1 END, length(name) ASC
		LIMIT 1`, args,
		&file, &line)
	if err != nil {
		return nil, err
	}
	return d.findMethodForRoute(file, line)
}

func (d *DB) findMethodForRoute(filePath string, line int) (*Node, error) {
	node := &Node{}
	err := d.scanRow(`SELECT id,name,kind,qualified_name,file_path,language,start_line,end_line,COALESCE(signature,'')
FROM nodes WHERE file_path=? AND kind IN ('method','function')
AND ((start_line<=? AND end_line>=?) OR (start_line>? AND start_line<=?))
ORDER BY CASE WHEN start_line<=? AND end_line>=? THEN 0 ELSE 1 END,abs(start_line-?) LIMIT 1`,
		[]any{filePath, line, line, line, line + 8, line, line, line},
		&node.ID, &node.Name, &node.Kind, &node.QualifiedName, &node.FilePath,
		&node.Language, &node.StartLine, &node.EndLine, &node.Signature)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// SearchSymbols bounds FTS work before applying deterministic in-process ranking.
func (d *DB) SearchSymbols(ctx context.Context, query SymbolQuery) ([]Node, error) {
	match := symbolMatch(query.Terms)
	if match == "" {
		return []Node{}, nil
	}
	if query.Limit <= 0 {
		query.Limit = 10
	}
	candidateLimit := min(max(query.Limit*4, 40), 200)
	var sqlText strings.Builder
	sqlText.WriteString(`SELECT n.id,n.name,n.kind,n.qualified_name,n.file_path,n.language,n.start_line,n.end_line,COALESCE(n.signature,'')
FROM nodes_fts JOIN nodes n ON n.rowid=nodes_fts.rowid WHERE nodes_fts MATCH ?`)
	args := []any{match}
	if len(query.Kinds) > 0 {
		sqlText.WriteString(" AND n.kind IN (")
		sqlText.WriteString(placeholders(len(query.Kinds)))
		sqlText.WriteByte(')')
		for _, kind := range query.Kinds {
			args = append(args, kind)
		}
	}
	if len(query.PathPrefixes) > 0 {
		sqlText.WriteString(" AND (")
		for i, prefix := range query.PathPrefixes {
			if i > 0 {
				sqlText.WriteString(" OR ")
			}
			sqlText.WriteString("n.file_path = ? OR (n.file_path >= ? AND n.file_path < ?)")
			prefix = strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/")
			args = append(args, prefix, prefix+"/", prefix+"0")
		}
		sqlText.WriteByte(')')
	}
	sqlText.WriteString(" LIMIT ?")
	args = append(args, candidateLimit)

	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := d.db.QueryContext(ctx, sqlText.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("codegraph search symbols: %w", err)
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, fmt.Errorf("codegraph scan symbol results: %w", err)
	}
	rankSymbols(nodes, query.Terms)
	if len(nodes) > query.Limit {
		nodes = nodes[:query.Limit]
	}
	return nodes, nil
}

// FindCallableNear resolves a callable whose declaration is at or immediately
// after an endpoint annotation. This is needed for Python decorators, where
// the endpoint line precedes the function declaration.
func (d *DB) FindCallableNear(ctx context.Context, filePath string, line int) (*Node, error) {
	filePath = strings.Trim(strings.ReplaceAll(filePath, "\\", "/"), "/")
	node := &Node{}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.db == nil {
		return nil, sql.ErrConnDone
	}
	err := d.db.QueryRowContext(ctx, `SELECT id,name,kind,qualified_name,file_path,language,start_line,end_line,COALESCE(signature,'')
FROM nodes WHERE file_path=? AND kind IN ('method','function')
AND ((start_line<=? AND end_line>=?) OR (start_line>? AND start_line<=?))
ORDER BY CASE WHEN start_line<=? AND end_line>=? THEN 0 ELSE 1 END,abs(start_line-?) LIMIT 1`,
		filePath, line, line, line, line+8, line, line, line).Scan(
		&node.ID, &node.Name, &node.Kind, &node.QualifiedName, &node.FilePath,
		&node.Language, &node.StartLine, &node.EndLine, &node.Signature)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// FindNodeAt finds the narrowest callable symbol containing an exact location.
func (d *DB) FindNodeAt(ctx context.Context, filePath string, line int) (*Node, error) {
	filePath = strings.Trim(strings.ReplaceAll(filePath, "\\", "/"), "/")
	node := &Node{}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.db == nil {
		return nil, sql.ErrConnDone
	}
	err := d.db.QueryRowContext(ctx, `SELECT id,name,kind,qualified_name,file_path,language,start_line,end_line,COALESCE(signature,'')
FROM nodes WHERE file_path=? AND start_line<=? AND end_line>=?
ORDER BY CASE kind WHEN 'method' THEN 0 WHEN 'function' THEN 0 WHEN 'route' THEN 1 ELSE 2 END,
(end_line-start_line) ASC LIMIT 1`, filePath, line, line).Scan(
		&node.ID, &node.Name, &node.Kind, &node.QualifiedName, &node.FilePath,
		&node.Language, &node.StartLine, &node.EndLine, &node.Signature)
	if err != nil {
		return nil, err
	}
	return node, nil
}

func symbolMatch(terms []string) string {
	seen := make(map[string]struct{}, len(terms))
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		for _, token := range strings.FieldsFunc(term, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
		}) {
			token = strings.ToLower(token)
			if len(token) < 2 {
				continue
			}
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			parts = append(parts, `"`+strings.ReplaceAll(token, `"`, `""`)+`"*`)
		}
	}
	return strings.Join(parts, " OR ")
}

func placeholders(count int) string {
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

func rankSymbols(nodes []Node, terms []string) {
	tokens := symbolTokens(terms)
	sort.SliceStable(nodes, func(i, j int) bool {
		left := symbolRank(nodes[i], tokens)
		right := symbolRank(nodes[j], tokens)
		if left != right {
			return left > right
		}
		if nodes[i].FilePath != nodes[j].FilePath {
			return nodes[i].FilePath < nodes[j].FilePath
		}
		if nodes[i].StartLine != nodes[j].StartLine {
			return nodes[i].StartLine < nodes[j].StartLine
		}
		return nodes[i].ID < nodes[j].ID
	})
}

func symbolTokens(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	tokens := make([]string, 0, len(terms))
	for _, term := range terms {
		for _, token := range strings.FieldsFunc(term, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
		}) {
			token = strings.ToLower(token)
			if len(token) < 2 {
				continue
			}
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func symbolRank(node Node, terms []string) int {
	name := strings.ToLower(node.Name)
	qualified := strings.ToLower(node.QualifiedName)
	score := callableKindRank(node.Kind)
	for _, term := range terms {
		switch {
		case name == term:
			score += 100
		case strings.HasPrefix(name, term):
			score += 60
		case strings.Contains(name, term):
			score += 35
		case strings.Contains(qualified, term):
			score += 15
		}
	}
	return score
}

func callableKindRank(kind string) int {
	switch kind {
	case "method", "function":
		return 6
	case "class", "interface":
		return 4
	case "route":
		return 2
	default:
		return 0
	}
}

// ListChunkNodes returns method/function nodes with source line ranges for
// semantic code chunking, ordered by file then start line.
func (d *DB) ListChunkNodes() ([]Node, error) {
	rows, err := d.query(`SELECT id, name, kind, qualified_name, file_path, language, start_line, end_line, COALESCE(signature,'')
		FROM nodes WHERE kind IN ('method','function') AND end_line >= start_line
		ORDER BY file_path, start_line`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func scanNodes(rows *sql.Rows) ([]Node, error) {
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.Kind, &n.QualifiedName, &n.FilePath, &n.Language, &n.StartLine, &n.EndLine, &n.Signature); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// DistinctNodeFilePaths returns all distinct file paths under repos/ in one
// query, so the caller can bucket them by repo client-side instead of running
// one count query per repo.
func (d *DB) DistinctNodeFilePaths() ([]string, error) {
	rows, err := d.query(`SELECT DISTINCT file_path FROM nodes WHERE file_path LIKE 'repos/%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}
