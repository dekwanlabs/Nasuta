package callchain

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

const (
	defaultDepth  = 3
	defaultNodes  = 40
	defaultFanout = 20
)

// Request identifies a symbol and the bounded traversal to run.
type Request struct {
	Query         string
	File          string
	Line          int
	QualifiedName string
	Direction     string
	MaxDepth      int
	MaxNodes      int
	MaxFanout     int
}

// Node adds canonical service ownership to a codegraph symbol.
type Node struct {
	codegraph.Node
	ServiceKey  string `json:"serviceKey,omitempty"`
	ServiceName string `json:"service,omitempty"`
}

// Hop is one verified local call or service-boundary bridge.
type Hop struct {
	Source Node           `json:"source"`
	Target Node           `json:"target"`
	Edge   codegraph.Edge `json:"edge"`
	Depth  int            `json:"depth"`
	Bridge bool           `json:"bridge,omitempty"`
}

// DirectionResult reports one bounded side of the graph.
type DirectionResult struct {
	Hops         []Hop    `json:"hops"`
	Truncated    bool     `json:"truncated"`
	NextFrontier []string `json:"nextFrontier,omitempty"`
	Unresolved   []string `json:"unresolved,omitempty"`
}

// Result is shared by the agent tool and Dashboard endpoint.
type Result struct {
	Target     *Node           `json:"target,omitempty"`
	Candidates []Node          `json:"candidates,omitempty"`
	Direction  string          `json:"direction"`
	MaxDepth   int             `json:"maxDepth"`
	MaxNodes   int             `json:"maxNodes"`
	MaxFanout  int             `json:"maxFanout"`
	Callers    DirectionResult `json:"callers"`
	Callees    DirectionResult `json:"callees"`
}

// Service closes method calls over the structured service index.
type Service struct {
	structure *store.SQLite
	graphMu   sync.RWMutex
	graph     *codegraph.DB
}

// New builds the shared method-call application service.
func New(structure *store.SQLite, graph *codegraph.DB) *Service {
	return &Service{structure: structure, graph: graph}
}

// SetGraph enables a database produced by a later full rebuild.
func (service *Service) SetGraph(graph *codegraph.DB) {
	service.graphMu.Lock()
	defer service.graphMu.Unlock()
	service.graph = graph
}

// Close releases the shared codegraph connection.
func (service *Service) Close() error {
	service.graphMu.Lock()
	graph := service.graph
	service.graph = nil
	service.graphMu.Unlock()
	if graph == nil {
		return nil
	}
	return graph.Close()
}

// Available reports whether function-level traversal can run.
func (service *Service) Available() bool {
	return service.structure != nil && service.graphDB() != nil
}

func (service *Service) graphDB() *codegraph.DB {
	service.graphMu.RLock()
	defer service.graphMu.RUnlock()
	return service.graph
}

// Trace resolves the explicit start point and traverses requested directions.
func (service *Service) Trace(ctx context.Context, request Request) (Result, error) {
	request = withDefaults(request)
	result := Result{
		Direction: request.Direction, MaxDepth: request.MaxDepth,
		MaxNodes: request.MaxNodes, MaxFanout: request.MaxFanout,
		Callers: DirectionResult{Hops: []Hop{}},
		Callees: DirectionResult{Hops: []Hop{}},
	}
	if !service.Available() {
		return result, fmt.Errorf("call chain unavailable: codegraph or structure index is not ready")
	}
	target, candidates, err := service.resolve(ctx, request)
	if err != nil {
		return result, err
	}
	if target == nil {
		result.Candidates = candidates
		return result, nil
	}
	owned := service.ownedNode(ctx, *target)
	result.Target = &owned
	if request.Direction == "callers" || request.Direction == "both" {
		result.Callers, err = service.traceDirection(ctx, *target, "callers", request)
		if err != nil {
			return result, err
		}
	}
	if request.Direction == "callees" || request.Direction == "both" {
		result.Callees, err = service.traceDirection(ctx, *target, "callees", request)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func withDefaults(request Request) Request {
	if request.Direction != "callers" && request.Direction != "callees" && request.Direction != "both" {
		request.Direction = "both"
	}
	if request.MaxDepth <= 0 {
		request.MaxDepth = defaultDepth
	}
	request.MaxDepth = min(request.MaxDepth, 8)
	if request.MaxNodes <= 0 {
		request.MaxNodes = defaultNodes
	}
	request.MaxNodes = min(request.MaxNodes, 200)
	if request.MaxFanout <= 0 {
		request.MaxFanout = defaultFanout
	}
	request.MaxFanout = min(request.MaxFanout, 100)
	return request
}

func (service *Service) resolve(ctx context.Context, request Request) (*codegraph.Node, []Node, error) {
	if request.File != "" && request.Line > 0 {
		node, err := service.graphDB().FindNodeAt(ctx, request.File, request.Line)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve call-chain location %s:%d: %w", request.File, request.Line, err)
		}
		// Decorator/annotation lines in Python often belong to the file node;
		// anchor them to the callable immediately below the declaration so
		// upstream endpoint traversal can continue into the function body.
		if node != nil && node.Kind == "file" {
			if callable, callableErr := service.graphDB().FindCallableNear(ctx, request.File, request.Line); callableErr == nil {
				node = callable
			}
		}
		return node, nil, nil
	}
	query := request.Query
	if strings.TrimSpace(query) == "" {
		query = request.QualifiedName
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil, fmt.Errorf("resolve call-chain symbol: query, qualified_name, or file+line is required")
	}
	nodes, err := service.graphDB().SearchSymbols(ctx, codegraph.SymbolQuery{
		Terms: symbolTerms(query), Limit: 40,
	})
	if err != nil {
		return nil, nil, err
	}
	callable := make([]codegraph.Node, 0, len(nodes))
	for _, node := range nodes {
		if !isCallable(node.Kind) || !matchesQualifier(node, request.File, request.QualifiedName) {
			continue
		}
		callable = append(callable, node)
	}
	if len(callable) == 0 {
		return nil, []Node{}, nil
	}
	if request.File != "" || request.QualifiedName != "" || !ambiguous(callable) {
		return &callable[0], nil, nil
	}
	candidates := make([]Node, 0, min(len(callable), 10))
	for _, node := range callable[:min(len(callable), 10)] {
		candidates = append(candidates, service.ownedNode(ctx, node))
	}
	return nil, candidates, nil
}

func isCallable(kind string) bool {
	switch kind {
	case "method", "function", "route", "class", "interface":
		return true
	default:
		return false
	}
}

func matchesQualifier(node codegraph.Node, file, qualifiedName string) bool {
	if file != "" && node.FilePath != file && !strings.HasSuffix(node.FilePath, "/"+strings.Trim(file, "/")) {
		return false
	}
	if qualifiedName != "" && !sameQualifiedName(node.QualifiedName, qualifiedName) {
		return false
	}
	return true
}

func sameQualifiedName(left, right string) bool {
	leftParts := qualifiedNameParts(left)
	rightParts := qualifiedNameParts(right)
	if len(leftParts) != len(rightParts) {
		return false
	}
	for i := range leftParts {
		if !strings.EqualFold(leftParts[i], rightParts[i]) {
			return false
		}
	}
	return len(leftParts) > 0
}

func qualifiedNameParts(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool {
		switch r {
		case '.', '#', '/', '\\', ':':
			return true
		default:
			return false
		}
	})
}

func ambiguous(nodes []codegraph.Node) bool {
	if len(nodes) < 2 {
		return false
	}
	return strings.EqualFold(nodes[0].Name, nodes[1].Name)
}

func symbolTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '.', '#', '/', '\\', ':':
			return true
		default:
			return false
		}
	})
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if len([]rune(field)) >= 2 {
			terms = append(terms, field)
		}
	}
	return terms
}

type frontierNode struct {
	node  codegraph.Node
	depth int
}

func (service *Service) traceDirection(ctx context.Context, root codegraph.Node, direction string, request Request) (DirectionResult, error) {
	result := DirectionResult{Hops: []Hop{}}
	visited := map[string]struct{}{root.ID: {}}
	bridged := make(map[string]struct{})
	frontierSeen := make(map[string]struct{})
	queue := []frontierNode{{node: root}}
	// A controller method can perform a direct WebClient/RestTemplate/HTTP
	// call without a codegraph "calls" edge for the fluent client chain. Seed
	// the bridge from the requested root so those outbound calls are not lost.
	if direction == "callees" {
		service.addDownstreamBridge(ctx, root, 0, request, &result, visited, bridged, &queue, frontierSeen)
	}
	if direction == "callers" {
		service.addUpstreamBridges(ctx, frontierNode{node: root}, request, &result, visited, bridged, &queue, frontierSeen)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= request.MaxDepth {
			hops, _, err := service.graphDB().CallEdges(current.node.ID, direction, 1)
			if err != nil {
				return DirectionResult{}, err
			}
			if len(hops) > 0 {
				result.Truncated = true
				addString(&result.NextFrontier, frontierSeen, current.node.ID)
			}
			continue
		}
		hops, more, err := service.graphDB().CallEdges(current.node.ID, direction, request.MaxFanout)
		if err != nil {
			return DirectionResult{}, err
		}
		if more {
			result.Truncated = true
			addString(&result.NextFrontier, frontierSeen, current.node.ID)
		}
		for _, graphHop := range hops {
			adjacent := graphHop.Target
			if direction == "callers" {
				adjacent = graphHop.Source
			}
			seen := has(visited, adjacent.ID)
			if !seen && len(visited) >= request.MaxNodes {
				result.Truncated = true
				addString(&result.NextFrontier, frontierSeen, current.node.ID)
				continue
			}
			result.Hops = append(result.Hops, service.ownedHop(ctx, graphHop, current.depth+1, false))
			if !seen {
				visited[adjacent.ID] = struct{}{}
				queue = append(queue, frontierNode{node: adjacent, depth: current.depth + 1})
			}
			if direction == "callees" {
				service.addDownstreamBridge(ctx, adjacent, current.depth+1, request, &result, visited, bridged, &queue, frontierSeen)
			}
		}
		if direction == "callers" {
			service.addUpstreamBridges(ctx, current, request, &result, visited, bridged, &queue, frontierSeen)
		}
	}
	return result, nil
}

func (service *Service) addDownstreamBridge(ctx context.Context, caller codegraph.Node, depth int, request Request, result *DirectionResult, visited, bridged map[string]struct{}, queue *[]frontierNode, frontierSeen map[string]struct{}) {
	if _, done := bridged[caller.ID]; done || depth >= request.MaxDepth {
		return
	}
	bridged[caller.ID] = struct{}{}
	edges, more, err := service.structure.DependenciesByEvidencePath(ctx, caller.FilePath, request.MaxFanout)
	if err != nil {
		result.Unresolved = append(result.Unresolved, fmt.Sprintf("dependency lookup for %s: %v", caller.FilePath, err))
		return
	}
	if more {
		result.Truncated = true
		addString(&result.NextFrontier, frontierSeen, caller.ID)
	}
	for _, dependency := range edges {
		if dependency.TargetKind != domain.DependencyTargetService {
			continue
		}
		method, path, ok := service.routeForDependency(caller, dependency)
		if !ok {
			result.Unresolved = append(result.Unresolved, fmt.Sprintf("route unresolved at %s:%d", caller.FilePath, caller.StartLine))
			continue
		}
		targetService, err := service.structure.ServiceByKey(ctx, dependency.TargetServiceKey)
		if err != nil {
			result.Unresolved = append(result.Unresolved, fmt.Sprintf("target service %s: %v", dependency.To, err))
			continue
		}
		targetPrefix := "repos/" + targetService.Repo
		if targetService.ModulePath != "." {
			targetPrefix += "/" + targetService.ModulePath
		}
		implementation, err := service.graphDB().ResolveDownstreamMethodInPath(targetPrefix, method, path)
		// The codegraph route adapter is intentionally language-specific. Python
		// FastAPI routers, for example, have endpoint metadata in the structure
		// index but no synthetic route node. Use the same canonical endpoint
		// record as the dashboard and anchor the bridge to its function node.
		if err != nil && dependency.Type == domain.EdgeHTTP {
			implementation, err = service.resolveEndpointMethod(ctx, targetService.ServiceKey, method, path)
		}
		if err != nil {
			result.Unresolved = append(result.Unresolved, fmt.Sprintf("%s %s -> %s: %v", method, path, dependency.To, err))
			continue
		}
		if !service.reserveNode(caller.ID, implementation.ID, request.MaxNodes, visited, result, frontierSeen) {
			continue
		}
		bridge := codegraph.CallHop{Source: caller, Target: *implementation, Depth: depth + 1, Edge: codegraph.Edge{
			Source: caller.ID, Target: implementation.ID, Kind: "service_route", Line: caller.StartLine,
			Confidence: dependency.Confidence, Provenance: string(dependency.Type),
		}}
		result.Hops = append(result.Hops, service.ownedHop(ctx, bridge, depth+1, true))
		*queue = append(*queue, frontierNode{node: *implementation, depth: depth + 1})
	}
}

func (service *Service) resolveEndpointMethod(ctx context.Context, serviceKey, method, path string) (*codegraph.Node, error) {
	owner, err := service.structure.ServiceByKey(ctx, serviceKey)
	if err != nil {
		return nil, err
	}
	page, err := service.structure.ListApis(ctx, owner.ServiceName, path, 1, 100)
	if err != nil {
		return nil, err
	}
	for _, endpoint := range page.List {
		if !strings.EqualFold(endpoint.Method, method) || endpoint.Path != path {
			continue
		}
		node, err := service.graphDB().FindCallableNear(ctx, endpoint.File, endpoint.Line)
		if err == nil && (node.Kind == "function" || node.Kind == "method") {
			return node, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (service *Service) addUpstreamBridges(ctx context.Context, current frontierNode, request Request, result *DirectionResult, visited, bridged map[string]struct{}, queue *[]frontierNode, frontierSeen map[string]struct{}) {
	if _, done := bridged[current.node.ID]; done || current.depth >= request.MaxDepth {
		return
	}
	bridged[current.node.ID] = struct{}{}
	owner, err := service.structure.ServiceForPath(ctx, current.node.FilePath)
	if err != nil {
		return
	}
	endpoint, err := service.structure.EndpointNearNode(ctx, owner.ServiceKey, current.node.FilePath, current.node.StartLine, current.node.EndLine)
	if err != nil {
		if err != sql.ErrNoRows {
			result.Unresolved = append(result.Unresolved, fmt.Sprintf("endpoint lookup for %s: %v", current.node.FilePath, err))
		}
		return
	}
	dependencies, more, err := service.structure.IncomingDependencies(ctx, owner.ServiceKey, request.MaxFanout)
	if err != nil {
		result.Unresolved = append(result.Unresolved, fmt.Sprintf("upstream lookup for %s: %v", owner.ServiceName, err))
		return
	}
	if more {
		result.Truncated = true
		addString(&result.NextFrontier, frontierSeen, current.node.ID)
	}
	// Prefer an exact static HTTP route over a coarse Feign dependency when
	// both mechanisms point at the same target service. Otherwise a Feign
	// repository fallback can incorrectly claim a WebClient proxy controller.
	sort.SliceStable(dependencies, func(i, j int) bool {
		return dependencies[i].Type == domain.EdgeHTTP && dependencies[j].Type != domain.EdgeHTTP
	})
	for _, dependency := range dependencies {
		if dependency.TargetKind != domain.DependencyTargetService ||
			dependency.CallerServiceKey == owner.ServiceKey {
			continue
		}
		callerService, err := service.structure.ServiceByKey(ctx, dependency.CallerServiceKey)
		if err != nil {
			addString(&result.Unresolved, frontierSeen, fmt.Sprintf("caller service %s: %v", dependency.From, err))
			continue
		}

		// Prefer an exact evidence file when it is itself an HTTP controller.
		// Maven-expanded Feign dependencies normally point at a shared API
		// interface instead; in that case resolve the matching proxy route in
		// the caller application's repository (including sibling modules).
		var caller *codegraph.Node
		bridgeLine := 0
		if dependency.Type == domain.EdgeHTTP {
			for _, evidence := range dependency.Evidence {
				outboundMethod, outboundPath, routeOK := service.graphDB().HTTPClientRouteAt(evidence.Path, evidence.Line)
				if !routeOK || !strings.EqualFold(outboundMethod, endpoint.Method) || outboundPath != endpoint.Path {
					continue
				}
				candidate, candidateErr := service.graphDB().FindCallableNear(ctx, evidence.Path, evidence.Line)
				if candidateErr == nil && (candidate.Kind == "function" || candidate.Kind == "method") {
					caller = candidate
					bridgeLine = evidence.Line
					break
				}
			}
		}
		if caller == nil {
			for _, evidence := range dependency.Evidence {
				caller, err = service.graphDB().ResolveRouteMethodInFile(evidence.Path, endpoint.Method, endpoint.Path)
				if err == nil {
					bridgeLine = evidence.Line
					break
				}
			}
		}
		if caller == nil {
			repoPrefix := "repos/" + callerService.Repo
			caller, err = service.graphDB().ResolveRouteMethodInPath(repoPrefix, endpoint.Method, endpoint.Path)
		}
		if err != nil || caller == nil {
			// A dependency may be a background consumer with no upstream HTTP
			// proxy. That is not a broken call-chain and should not flood the
			// dashboard with one unresolved item per shared interface file.
			continue
		}
		if !service.reserveNode(current.node.ID, caller.ID, request.MaxNodes, visited, result, frontierSeen) {
			continue
		}
		if bridgeLine == 0 {
			bridgeLine = caller.StartLine
		}
		bridge := codegraph.CallHop{Source: *caller, Target: current.node, Depth: current.depth + 1, Edge: codegraph.Edge{
			Source: caller.ID, Target: current.node.ID, Kind: "service_route", Line: bridgeLine,
			Confidence: dependency.Confidence, Provenance: string(dependency.Type),
		}}
		hop := service.ownedHop(ctx, bridge, current.depth+1, true)
		// The source file can belong to a library/controller module, while the
		// dependency caller is the deployable Spring Boot application. Keep the
		// source location but expose the canonical runtime owner to consumers.
		hop.Source.ServiceKey = callerService.ServiceKey
		hop.Source.ServiceName = callerService.ServiceName
		result.Hops = append(result.Hops, hop)
		*queue = append(*queue, frontierNode{node: *caller, depth: current.depth + 1})
	}
}

func (service *Service) routeForDependency(node codegraph.Node, dependency domain.DependencyEdge) (string, string, bool) {
	// A Feign dependency carries two different source locations: the real
	// caller invocation and the Feign method declaration. The caller's route is
	// its inbound API and must never be used as the downstream route. Resolve
	// the declared client method first; ResolveDownstreamMethod intentionally
	// accepts a path suffix, so a Feign method path such as /message can still
	// match the provider's /system/welcome/message route.
	if dependency.Type == domain.EdgeFeign {
		for _, evidence := range dependency.Evidence {
			if method, path, ok := service.graphDB().RouteAt(evidence.Path, evidence.Line); ok {
				return method, path, true
			}
		}
	}
	// HTTP client evidence describes the outbound route, which is different
	// from the caller controller's inbound route. Prefer it before looking at
	// the node route so WebClient/RestTemplate calls do not resolve against the
	// proxy endpoint by mistake.
	if dependency.Type == domain.EdgeHTTP {
		for _, evidence := range dependency.Evidence {
			if evidence.Path != node.FilePath {
				continue
			}
			if method, path, ok := service.graphDB().HTTPClientRouteAt(evidence.Path, evidence.Line); ok {
				return method, path, true
			}
		}
	}
	if method, path, ok := service.graphDB().RouteForNode(node); ok {
		return method, path, true
	}
	for _, evidence := range dependency.Evidence {
		if evidence.Path == node.FilePath {
			if method, path, ok := service.graphDB().RouteAt(evidence.Path, evidence.Line); ok {
				return method, path, true
			}
		}
	}
	return "", "", false
}

func (service *Service) reserveNode(fromID, nodeID string, maxNodes int, visited map[string]struct{}, result *DirectionResult, frontierSeen map[string]struct{}) bool {
	if has(visited, nodeID) {
		return false
	}
	if len(visited) >= maxNodes {
		result.Truncated = true
		addString(&result.NextFrontier, frontierSeen, fromID)
		return false
	}
	visited[nodeID] = struct{}{}
	return true
}

func (service *Service) ownedHop(ctx context.Context, hop codegraph.CallHop, depth int, bridge bool) Hop {
	return Hop{
		Source: service.ownedNode(ctx, hop.Source), Target: service.ownedNode(ctx, hop.Target),
		Edge: hop.Edge, Depth: depth, Bridge: bridge,
	}
}

func (service *Service) ownedNode(ctx context.Context, node codegraph.Node) Node {
	owned := Node{Node: node}
	owner, err := service.structure.ServiceForPath(ctx, node.FilePath)
	if err == nil {
		owned.ServiceKey = owner.ServiceKey
		owned.ServiceName = owner.ServiceName
	}
	return owned
}

func addString(values *[]string, seen map[string]struct{}, value string) {
	if has(seen, value) {
		return
	}
	seen[value] = struct{}{}
	*values = append(*values, value)
}

func has(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}
