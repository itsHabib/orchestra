package dag

import (
	"fmt"

	"github.com/itsHabib/orchestra/internal/config"
)

// BuildTiers takes agents and returns execution tiers using Kahn's algorithm.
// Each tier is a []string of agent names that can run in parallel.
// Returns error if there's a cycle.
func BuildTiers(agents []config.Agent) ([][]string, error) {
	if len(agents) == 0 {
		return nil, nil
	}

	// Build adjacency list and in-degree map
	inDegree := make(map[string]int)
	dependents := make(map[string][]string) // dep -> agents that depend on it

	for i := range agents {
		if _, ok := inDegree[agents[i].Name]; !ok {
			inDegree[agents[i].Name] = 0
		}
		for _, dep := range agents[i].DependsOn {
			inDegree[agents[i].Name]++
			dependents[dep] = append(dependents[dep], agents[i].Name)
		}
	}

	// Seed queue with zero in-degree nodes
	var queue []string
	for i := range agents {
		if inDegree[agents[i].Name] == 0 {
			queue = append(queue, agents[i].Name)
		}
	}

	var tiers [][]string
	processed := 0

	for len(queue) > 0 {
		tier := queue
		queue = nil
		tiers = append(tiers, tier)
		processed += len(tier)

		for _, name := range tier {
			for _, dep := range dependents[name] {
				inDegree[dep]--
				if inDegree[dep] == 0 {
					queue = append(queue, dep)
				}
			}
		}
	}

	if processed < len(agents) {
		return nil, fmt.Errorf("dependency cycle detected: processed %d of %d agents", processed, len(agents))
	}

	return tiers, nil
}
