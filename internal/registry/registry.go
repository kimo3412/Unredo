// Package registry compiles backend factories and resolves profiles to
// concrete ports.Backend instances. Core never imports a concrete backend
// directly; CLI goes through this package.
package registry

import (
	"fmt"
	"sync"

	"github.com/girimi/unredo/internal/config"
	"github.com/girimi/unredo/internal/ports"
)

// Factory builds a backend from a profile.
type Factory func(p *config.Profile) (ports.Backend, error)

var (
	mu       sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a backend factory available under the given name.
// It panics on duplicate registration because that is a programmer error
// at link time, not a runtime condition to surface to users.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("registry: backend %q already registered", name))
	}
	factories[name] = f
}

// Resolve builds the named backend for the given profile.
func Resolve(p *config.Profile) (ports.Backend, error) {
	mu.RLock()
	f, ok := factories[p.Backend]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("registry: backend %q not registered", p.Backend)
	}
	return f(p)
}

// Names returns the list of registered backend names. Useful for help text.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	return out
}
