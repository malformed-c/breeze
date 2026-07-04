package engine

import "fmt"

// validateEnvironmentDeps checks Pipeline.EnvironmentDeps at registration time: every
// key/value must be a declared environment, no self-reference, and the graph must be
// acyclic (DFS) — a cyclic config is rejected outright here, never reachable at run time.
func validateEnvironmentDeps(envs []string, deps map[string][]string) error {
	known := make(map[string]bool, len(envs))
	for _, e := range envs {
		known[e] = true
	}
	for env, dependsOn := range deps {
		if !known[env] {
			return fmt.Errorf("environment_deps references unknown environment %q", env)
		}
		for _, dep := range dependsOn {
			if !known[dep] {
				return fmt.Errorf("environment %q depends on unknown environment %q", env, dep)
			}
			if dep == env {
				return fmt.Errorf("environment %q cannot depend on itself", env)
			}
		}
	}

	const (
		white = 0 // unvisited
		gray  = 1 // in progress (on current DFS stack)
		black = 2 // fully processed
	)
	color := make(map[string]int, len(envs))
	var visit func(node string, stack []string) error
	visit = func(node string, stack []string) error {
		color[node] = gray
		for _, dep := range deps[node] {
			switch color[dep] {
			case gray:
				return fmt.Errorf("cyclic environment_deps: %v -> %s", append(stack, node, dep), dep)
			case white:
				if err := visit(dep, append(stack, node)); err != nil {
					return err
				}
			}
		}
		color[node] = black
		return nil
	}
	for _, e := range envs {
		if color[e] == white {
			if err := visit(e, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
