package apiextract

import "sync"

// ExtractorRegistry holds the fleet of registered provider and consumer
// extractors. It is the single place where concrete extractor packages
// are wired at daemon startup — scanner and dog code only ever see the
// registry interface, never the concrete types.
//
// Thread-safety: RegisterProvider/RegisterConsumer are called once at
// startup before any goroutine reads the registry, but RWMutex is
// included so test helpers can register extractors concurrently without
// data races.
type ExtractorRegistry struct {
	mu        sync.RWMutex
	providers []ProviderExtractor
	consumers []ConsumerExtractor
}

// NewExtractorRegistry returns an empty, ready-to-use registry.
func NewExtractorRegistry() *ExtractorRegistry {
	return &ExtractorRegistry{}
}

// RegisterProvider adds a ProviderExtractor to the registry.
// Callers (daemon startup only) invoke this once per extractor at boot time.
func (r *ExtractorRegistry) RegisterProvider(e ProviderExtractor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, e)
}

// RegisterConsumer adds a ConsumerExtractor to the registry.
// Callers (daemon startup only) invoke this once per extractor at boot time.
func (r *ExtractorRegistry) RegisterConsumer(e ConsumerExtractor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consumers = append(r.consumers, e)
}

// AllProviders returns a snapshot of the registered ProviderExtractors.
// Safe to call from multiple goroutines.
func (r *ExtractorRegistry) AllProviders() []ProviderExtractor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderExtractor, len(r.providers))
	copy(out, r.providers)
	return out
}

// AllConsumers returns a snapshot of the registered ConsumerExtractors.
// Safe to call from multiple goroutines.
func (r *ExtractorRegistry) AllConsumers() []ConsumerExtractor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ConsumerExtractor, len(r.consumers))
	copy(out, r.consumers)
	return out
}
