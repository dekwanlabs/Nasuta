package ontology

import "context"

type neighborReader interface {
	Neighbors(context.Context, NeighborQuery) ([]Fact, bool, error)
}

func FindBoundedPaths(ctx context.Context, repository neighborReader, query PathQuery) ([]Path, bool, error) {
	if err := ValidatePathQuery(query); err != nil {
		return nil, false, err
	}
	discoveredDepth := map[string]int{query.StartID: 0}
	parents := make(map[string]pathParent, query.MaxNodes)
	frontier := []string{query.StartID}
	paths := make([]Path, 0)
	truncated := false

	for depth := 1; depth <= query.MaxDepth && len(frontier) > 0; depth++ {
		limit := min(200, query.MaxNodes-len(discoveredDepth)+len(frontier)*query.MaxFanout)
		if limit < 1 {
			return paths, true, nil
		}
		facts, more, err := repository.Neighbors(ctx, NeighborQuery{
			EntityIDs: frontier, Predicates: query.Predicates, Direction: query.Direction, Limit: limit,
			Generation: query.Generation,
		})
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || more
		frontierSet := make(map[string]struct{}, len(frontier))
		fanout := make(map[string]int, len(frontier))
		for _, entityID := range frontier {
			frontierSet[entityID] = struct{}{}
		}
		next := make([]string, 0)
		for _, fact := range facts {
			from, to, ok := traversalEdge(fact, frontierSet, query.Direction)
			if !ok {
				continue
			}
			if fanout[from] >= query.MaxFanout {
				truncated = true
				continue
			}
			fanout[from]++
			if previousDepth, seen := discoveredDepth[to]; seen {
				if previousDepth == depth {
					paths = append(paths, extendPath(query.StartID, from, to, fact, parents))
				}
				continue
			}
			if len(discoveredDepth) >= query.MaxNodes {
				truncated = true
				continue
			}
			discoveredDepth[to] = depth
			parents[to] = pathParent{from: from, fact: fact}
			next = append(next, to)
			path := buildPath(query.StartID, to, parents)
			paths = append(paths, path)
			if query.TargetID != "" && to == query.TargetID {
				return []Path{path}, truncated, nil
			}
		}
		frontier = next
	}
	if len(frontier) > 0 {
		truncated = true
	}
	if query.TargetID != "" {
		return []Path{}, truncated, nil
	}
	return paths, truncated, nil
}

func extendPath(startID, fromID, targetID string, fact Fact, parents map[string]pathParent) Path {
	if fromID == startID {
		return Path{EntityIDs: []string{startID, targetID}, Facts: []Fact{fact}}
	}
	path := buildPath(startID, fromID, parents)
	path.EntityIDs = append(path.EntityIDs, targetID)
	path.Facts = append(path.Facts, fact)
	return path
}

type pathParent struct {
	from string
	fact Fact
}

func traversalEdge(fact Fact, frontier map[string]struct{}, direction Direction) (string, string, bool) {
	if direction != DirectionIncoming {
		if _, ok := frontier[fact.SubjectID]; ok {
			return fact.SubjectID, fact.ObjectID, true
		}
	}
	if direction != DirectionOutgoing {
		if _, ok := frontier[fact.ObjectID]; ok {
			return fact.ObjectID, fact.SubjectID, true
		}
	}
	return "", "", false
}

func buildPath(startID, targetID string, parents map[string]pathParent) Path {
	entities := []string{targetID}
	facts := make([]Fact, 0)
	for current := targetID; current != startID; {
		parent := parents[current]
		facts = append(facts, parent.fact)
		current = parent.from
		entities = append(entities, current)
	}
	for left, right := 0, len(entities)-1; left < right; left, right = left+1, right-1 {
		entities[left], entities[right] = entities[right], entities[left]
	}
	for left, right := 0, len(facts)-1; left < right; left, right = left+1, right-1 {
		facts[left], facts[right] = facts[right], facts[left]
	}
	return Path{EntityIDs: entities, Facts: facts}
}
