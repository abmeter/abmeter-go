package abmeter

import (
	"fmt"
	"time"
)

// Exposure is the record built for every resolution. It is submitted to the
// platform only when the resolution came from an experiment or feature flag
// (ExposableID non-nil); the default-value path carries nil identity fields
// and is never submitted. Field names are the exact wire format of
// POST /api/v1/exposures.
type Exposure struct {
	ParameterID   int     `json:"parameter_id"`
	SpaceID       int     `json:"space_id"`
	ResolvedValue any     `json:"resolved_value"`
	UserID        *string `json:"user_id"`
	ExposableType *string `json:"exposable_type"`
	ExposableID   *int    `json:"exposable_id"`
	AudienceID    *int    `json:"audience_id"`
	ResolvedAt    string  `json:"resolved_at"`
}

const (
	exposableTypeFeatureFlag = "FeatureFlag"
	exposableTypeExperiment  = "Experiment"
)

// userParameterResolver resolves a user's value for a parameter.
// Resolution order: feature flag -> experiment -> parameter default.
type userParameterResolver struct {
	config *assignmentConfig
}

func newResolver(configJSON []byte) (*userParameterResolver, error) {
	config, err := parseAssignmentConfig(configJSON)
	if err != nil {
		return nil, err
	}
	return &userParameterResolver{config: config}, nil
}

func (r *userParameterResolver) exposureFor(user User, parameterSlug string) (Exposure, error) {
	if user.UserID == "" {
		return Exposure{}, fmt.Errorf("user must have a user_id")
	}

	param, err := r.findParameter(parameterSlug)
	if err != nil {
		return Exposure{}, err
	}

	if flag := r.matchingFeatureFlag(user, parameterSlug); flag != nil {
		return makeExposure(user, param, exposableTypeFeatureFlag, flag.id, flag.audience.audienceID(), flag.variant), nil
	}

	if variant, exp, audienceID, ok := r.matchingExperimentVariant(user, parameterSlug); ok {
		return makeExposure(user, param, exposableTypeExperiment, exp.id, audienceID, variant), nil
	}

	return Exposure{
		ParameterID:   param.id,
		SpaceID:       param.spaceID,
		ResolvedValue: param.defaultValue,
		ResolvedAt:    nowISO8601(),
	}, nil
}

func (r *userParameterResolver) findParameter(slug string) (*parameter, error) {
	for i := range r.config.parameters {
		if r.config.parameters[i].slug == slug {
			return &r.config.parameters[i], nil
		}
	}
	return nil, fmt.Errorf("parameter %q not found", slug)
}

func (r *userParameterResolver) matchingFeatureFlag(user User, parameterSlug string) *featureFlag {
	for i := range r.config.featureFlags {
		flag := &r.config.featureFlags[i]
		if flag.audience.matches(user) && flag.variant != nil && flag.variant.controlsParameter(parameterSlug) {
			return flag
		}
	}
	return nil
}

// matchingExperimentVariant finds the experiment audience/variant the user is
// allocated to: allocation into the experiment via the space salt, then
// audience membership via the experiment's own salt.
func (r *userParameterResolver) matchingExperimentVariant(user User, parameterSlug string) (*variant, *experiment, int, bool) {
	for i := range r.config.experiments {
		exp := &r.config.experiments[i]
		if !experimentControlsParameter(exp, parameterSlug) {
			continue
		}

		experimentPercentage := toPercentage(exp.spaceSalt, user.UserID)
		if !exp.rng.contains(experimentPercentage) {
			continue
		}

		userPercentage := toPercentage(exp.salt, user.UserID)
		for _, av := range exp.audienceVariants {
			if random, ok := av.audience.(*randomAudience); ok && random.rng.contains(userPercentage) {
				return av.variant, exp, av.audience.audienceID(), true
			}
		}
	}
	return nil, nil, 0, false
}

func experimentControlsParameter(exp *experiment, parameterSlug string) bool {
	for _, av := range exp.audienceVariants {
		if av.variant != nil && av.variant.controlsParameter(parameterSlug) {
			return true
		}
	}
	return false
}

// makeExposure resolves the value (variant override wins over the parameter
// default whenever the variant sets the slug — including explicitly-falsy
// values; only an unset slug falls through) and builds the full record.
func makeExposure(user User, param *parameter, exposableType string, exposableID, audienceID int, v *variant) Exposure {
	value := param.defaultValue
	if v != nil {
		if override, ok := v.parameterValue(param.slug); ok && override != nil {
			value = override
		}
	}

	userID := user.UserID
	return Exposure{
		ParameterID:   param.id,
		SpaceID:       param.spaceID,
		ResolvedValue: value,
		UserID:        &userID,
		ExposableType: &exposableType,
		ExposableID:   &exposableID,
		AudienceID:    &audienceID,
		ResolvedAt:    nowISO8601(),
	}
}

func nowISO8601() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
