package dag

import (
	"fmt"

	"github.com/itsHabib/orchestra/internal/config"
)

// BuildTiers takes teams and returns execution tiers using Kahn's algorithm.
// Each tier is a []string of team names that can run in parallel.
// Returns error if there's a cycle.
func BuildTiers(teams []config.Team) ([][]string, error) {
	if len(teams) == 0 {
		return nil, nil
	}

	// Build adjacency list and in-degree map
	inDegree := make(map[string]int)
	dependents := make(map[string][]string) // dep -> teams that depend on it

	for i := range teams {
		if _, ok := inDegree[teams[i].Name]; !ok {
			inDegree[teams[i].Name] = 0
		}
		for _, dep := range teams[i].DependsOn {
			inDegree[teams[i].Name]++
			dependents[dep] = append(dependents[dep], teams[i].Name)
		}
	}

	// Seed queue with zero in-degree nodes
	var queue []string
	for i := range teams {
		if inDegree[teams[i].Name] == 0 {
			queue = append(queue, teams[i].Name)
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

	if processed < len(teams) {
		return nil, fmt.Errorf("dependency cycle detected: processed %d of %d teams", processed, len(teams))
	}

	return tiers, nil
}
