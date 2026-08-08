package abmeter

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Assignment-config value objects, deserialize-only: only the platform emits
// config (`GET /api/v1/assignment-config`); a client only ever parses it.
// Serialization, range precomputation (percentages_to_ranges), and the
// build-from-DB path are server-side concerns and intentionally absent.

// valueRange is an inclusive integer range: both begin and end are members.
type valueRange struct {
	begin int
	end   int
}

func (r valueRange) contains(v int) bool {
	return r.begin <= v && v <= r.end
}

type space struct {
	id   int
	salt string
}

type parameter struct {
	id            int
	slug          string
	parameterType string
	defaultValue  any
	spaceID       int
}

type variant struct {
	id              int
	parameterValues map[string]any
}

// parameterValue returns the variant's value for the slug and whether the
// variant sets it at all. The two-value form is what preserves the falsy
// override: an explicitly-set false/0/"" is a real override and must win over
// the parameter default — only an *unset* slug falls through.
func (v *variant) parameterValue(slug string) (any, bool) {
	value, ok := v.parameterValues[slug]
	return value, ok
}

func (v *variant) controlsParameter(slug string) bool {
	_, ok := v.parameterValues[slug]
	return ok
}

// audience is implemented by the three platform audience kinds. Implementing
// the interface forces the matches override at compile time.
type audience interface {
	audienceID() int
	matches(user User) bool
}

type userListAudience struct {
	id      int
	userIDs []string
}

func (a *userListAudience) audienceID() int { return a.id }

func (a *userListAudience) matches(user User) bool {
	for _, id := range a.userIDs {
		if id == user.UserID {
			return true
		}
	}
	return false
}

type predicateAudience struct {
	id        int
	predicate *regexp.Regexp
}

func (a *predicateAudience) audienceID() int { return a.id }

// matches searches the unanchored predicate against the email. A missing
// email is a silent no-match: email is optional platform-wide.
func (a *predicateAudience) matches(user User) bool {
	if user.Email == "" {
		return false
	}
	return a.predicate.MatchString(user.Email)
}

type randomAudience struct {
	id  int
	rng valueRange
}

func (a *randomAudience) audienceID() int { return a.id }

// matches is false: random audiences allocate via their range against the
// user's experiment-salt percentage, handled by the resolver.
func (a *randomAudience) matches(User) bool { return false }

// audienceVariant pairs an audience with its variant; variant is nil for
// control (the audience whose users see the parameter default).
type audienceVariant struct {
	audience audience
	variant  *variant
}

type featureFlag struct {
	id       int
	audience audience
	variant  *variant
}

type experiment struct {
	id               int
	rng              valueRange
	audienceVariants []audienceVariant
	spaceID          int
	salt             string
	spaceSalt        string
}

type assignmentConfig struct {
	spaces       []space
	parameters   []parameter
	featureFlags []featureFlag
	experiments  []experiment
}

// --- wire format -----------------------------------------------------------

type wireParameterValue struct {
	Slug  string `json:"slug"`
	Value any    `json:"value"`
}

type wireVariant struct {
	ID              int                  `json:"id"`
	ParameterValues []wireParameterValue `json:"parameter_values"`
}

type wireAudience struct {
	ID        int      `json:"id"`
	Type      string   `json:"type"`
	UserIDs   []string `json:"user_ids"`
	Predicate string   `json:"predicate"`
	Range     []int    `json:"range"`
}

type wireAudienceVariant struct {
	Audience wireAudience `json:"audience"`
	Variant  *wireVariant `json:"variant"`
}

type wireExperiment struct {
	ID               int                   `json:"id"`
	SpaceID          int                   `json:"space_id"`
	Salt             *string               `json:"salt"`
	Range            []int                 `json:"range"`
	AudienceVariants []wireAudienceVariant `json:"audience_variants"`
}

type wireFeatureFlag struct {
	ID       int          `json:"id"`
	Audience wireAudience `json:"audience"`
	Variant  wireVariant  `json:"variant"`
}

type wireParameter struct {
	ID            int    `json:"id"`
	Slug          string `json:"slug"`
	ParameterType string `json:"parameter_type"`
	DefaultValue  any    `json:"default_value"`
	SpaceID       int    `json:"space_id"`
}

type wireSpace struct {
	ID   int    `json:"id"`
	Salt string `json:"salt"`
}

type wireConfig struct {
	Spaces       []wireSpace       `json:"spaces"`
	Parameters   []wireParameter   `json:"parameters"`
	FeatureFlags []wireFeatureFlag `json:"feature_flags"`
	Experiments  []wireExperiment  `json:"experiments"`
}

// parseAssignmentConfig builds the config from the JSON payload the platform
// returns in the `config` field of GET /api/v1/assignment-config.
func parseAssignmentConfig(configJSON []byte) (*assignmentConfig, error) {
	var wire wireConfig
	if err := json.Unmarshal(configJSON, &wire); err != nil {
		return nil, fmt.Errorf("invalid assignment config: %w", err)
	}

	config := &assignmentConfig{}
	spaceSalts := make(map[int]string, len(wire.Spaces))
	for _, s := range wire.Spaces {
		config.spaces = append(config.spaces, space{id: s.ID, salt: s.Salt})
		spaceSalts[s.ID] = s.Salt
	}

	for _, p := range wire.Parameters {
		defaultValue, err := castValue(p.DefaultValue, p.ParameterType)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", p.Slug, err)
		}
		config.parameters = append(config.parameters, parameter{
			id:            p.ID,
			slug:          p.Slug,
			parameterType: p.ParameterType,
			defaultValue:  defaultValue,
			spaceID:       p.SpaceID,
		})
	}

	for _, f := range wire.FeatureFlags {
		aud, err := parseAudience(f.Audience)
		if err != nil {
			return nil, err
		}
		config.featureFlags = append(config.featureFlags, featureFlag{
			id:       f.ID,
			audience: aud,
			variant:  parseVariant(&f.Variant),
		})
	}

	for _, e := range wire.Experiments {
		exp, err := parseExperiment(e, spaceSalts)
		if err != nil {
			return nil, err
		}
		config.experiments = append(config.experiments, exp)
	}

	return config, nil
}

func parseExperiment(wire wireExperiment, spaceSalts map[int]string) (experiment, error) {
	spaceSalt, ok := spaceSalts[wire.SpaceID]
	if !ok {
		return experiment{}, fmt.Errorf("space with id %d not found", wire.SpaceID)
	}
	// The per-experiment salt is a required wire field. No fallback: a
	// defaulted salt would silently change every bucket assignment.
	if wire.Salt == nil {
		return experiment{}, fmt.Errorf("experiment %d: missing required salt", wire.ID)
	}
	rng, err := parseRange(wire.Range, fmt.Sprintf("experiment %d", wire.ID))
	if err != nil {
		return experiment{}, err
	}

	audienceVariants := make([]audienceVariant, 0, len(wire.AudienceVariants))
	for _, av := range wire.AudienceVariants {
		aud, err := parseAudience(av.Audience)
		if err != nil {
			return experiment{}, err
		}
		audienceVariants = append(audienceVariants, audienceVariant{
			audience: aud,
			variant:  parseVariant(av.Variant),
		})
	}

	return experiment{
		id:               wire.ID,
		rng:              rng,
		audienceVariants: audienceVariants,
		spaceID:          wire.SpaceID,
		salt:             *wire.Salt,
		spaceSalt:        spaceSalt,
	}, nil
}

func parseVariant(wire *wireVariant) *variant {
	if wire == nil {
		return nil
	}
	values := make(map[string]any, len(wire.ParameterValues))
	for _, pv := range wire.ParameterValues {
		values[pv.Slug] = pv.Value
	}
	return &variant{id: wire.ID, parameterValues: values}
}

func parseAudience(wire wireAudience) (audience, error) {
	switch wire.Type {
	case "user_list":
		return &userListAudience{id: wire.ID, userIDs: wire.UserIDs}, nil
	case "predicate":
		re, err := regexp.Compile(wire.Predicate)
		if err != nil {
			return nil, fmt.Errorf("audience %d: invalid predicate: %w", wire.ID, err)
		}
		return &predicateAudience{id: wire.ID, predicate: re}, nil
	case "random":
		rng, err := parseRange(wire.Range, fmt.Sprintf("audience %d", wire.ID))
		if err != nil {
			return nil, err
		}
		return &randomAudience{id: wire.ID, rng: rng}, nil
	default:
		return nil, fmt.Errorf("unknown audience type: %q", wire.Type)
	}
}

func parseRange(pair []int, owner string) (valueRange, error) {
	if len(pair) != 2 {
		return valueRange{}, fmt.Errorf("%s: range must be a [begin, end] pair", owner)
	}
	return valueRange{begin: pair[0], end: pair[1]}, nil
}
