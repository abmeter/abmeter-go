package abmeter

import (
	"errors"
	"sync"
	"time"
)

// resolverProvider owns the assignment-config lifecycle: fetch and cache the
// config on fetchInterval (with If-None-Match, so an unchanged config costs a
// 304 and keeps the current resolver), delegate resolution, and queue
// exposures for background submission.
type resolverProvider struct {
	mu            sync.Mutex
	apiClient     *client
	sub           *submitter
	staticConfig  string
	fetchInterval time.Duration

	resolver      *userParameterResolver
	etag          string
	lastFetchedAt time.Time
}

func newResolverProvider(cfg *Config, apiClient *client, sub *submitter) (*resolverProvider, error) {
	provider := &resolverProvider{
		apiClient:     apiClient,
		sub:           sub,
		staticConfig:  cfg.StaticConfig,
		fetchInterval: cfg.FetchInterval,
	}
	if cfg.StaticConfig != "" {
		resolver, err := newResolver([]byte(cfg.StaticConfig))
		if err != nil {
			return nil, err
		}
		provider.resolver = resolver
	}
	return provider, nil
}

func (p *resolverProvider) resolveParameter(user User, parameterSlug string) (any, error) {
	resolver, err := p.currentResolver()
	if err != nil {
		return nil, err
	}
	exposure, err := resolver.exposureFor(user, parameterSlug)
	if err != nil {
		return nil, err
	}
	if p.sub != nil && exposure.ExposableID != nil {
		p.sub.queueExposure(exposure)
	}
	return exposure.ResolvedValue, nil
}

// getExposure evaluates the full exposure detail without submitting it.
func (p *resolverProvider) getExposure(user User, parameterSlug string) (Exposure, error) {
	resolver, err := p.currentResolver()
	if err != nil {
		return Exposure{}, err
	}
	return resolver.exposureFor(user, parameterSlug)
}

func (p *resolverProvider) currentResolver() (*userParameterResolver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.staticConfig != "" {
		return p.resolver, nil
	}
	if p.resolver == nil || time.Since(p.lastFetchedAt) > p.fetchInterval {
		if err := p.refreshLocked(); err != nil {
			// Keep serving the previous config when a refresh fails.
			if p.resolver == nil {
				return nil, err
			}
			p.lastFetchedAt = time.Now()
		}
	}
	if p.resolver == nil {
		return nil, errors.New("configuration not loaded")
	}
	return p.resolver, nil
}

func (p *resolverProvider) refreshLocked() error {
	response, err := p.apiClient.getAssignmentConfig(p.etag)
	if err != nil {
		return err
	}
	if !response.notModified {
		resolver, err := newResolver(response.configJSON)
		if err != nil {
			return err
		}
		p.resolver = resolver
		p.etag = response.etag
	}
	p.lastFetchedAt = time.Now()
	return nil
}
