package buildcache

import "fmt"

// Registry manages available artifact generators.
// The router creates one at startup and registers all generators.
type Registry struct {
	generators map[string]ArtifactGenerator
}

// NewRegistry creates a new registry.
func NewRegistry() *Registry {
	return &Registry{generators: make(map[string]ArtifactGenerator)}
}

// Register adds a generator to the registry.
func (r *Registry) Register(gen ArtifactGenerator) {
	r.generators[gen.BuildSystem()] = gen
}

// FindGenerator returns the generator that matches the given ingestion type,
// or nil if none matches.
func (r *Registry) FindGenerator(ingestionType string) ArtifactGenerator {
	for _, gen := range r.generators {
		if gen.ShouldGenerate(ingestionType) {
			return gen
		}
	}
	return nil
}

// Get returns a generator by build system name.
func (r *Registry) Get(buildSystem string) (ArtifactGenerator, error) {
	gen, ok := r.generators[buildSystem]
	if !ok {
		return nil, fmt.Errorf("no artifact generator for %q", buildSystem)
	}
	return gen, nil
}

// Has returns true if a generator is registered for the given build system.
func (r *Registry) Has(buildSystem string) bool {
	_, ok := r.generators[buildSystem]
	return ok
}
