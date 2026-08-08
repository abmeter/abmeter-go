package abmeter

import (
	"strings"
	"testing"
)

func mustResolver(t *testing.T, configJSON string) *userParameterResolver {
	t.Helper()
	resolver, err := newResolver([]byte(configJSON))
	if err != nil {
		t.Fatalf("newResolver: %v", err)
	}
	return resolver
}

// findUserInRange scans for a user id whose two bucketing percentages land in
// the given experiment-allocation and audience ranges — keeps tests honest
// against the real hash instead of hand-picked magic ids.
func findUserInRange(t *testing.T, spaceSalt, salt string, allocation, audienceRange valueRange) string {
	t.Helper()
	for i := 0; i < 100000; i++ {
		id := "user_" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + itoa(i)
		if allocation.contains(toPercentage(spaceSalt, id)) && audienceRange.contains(toPercentage(salt, id)) {
			return id
		}
	}
	t.Fatal("no user id found for ranges")
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestResolveControlAndTreatment(t *testing.T) {
	resolver := mustResolver(t, testConfigJSON)

	controlUser := findUserInRange(t, "space-salt-1", "experiment-salt-20",
		valueRange{1, 100}, valueRange{1, 50})
	treatmentUser := findUserInRange(t, "space-salt-1", "experiment-salt-20",
		valueRange{1, 100}, valueRange{51, 100})

	control, err := resolver.exposureFor(User{UserID: controlUser}, "button-color")
	if err != nil {
		t.Fatalf("control resolve: %v", err)
	}
	// Control variant is null: the user is exposed (Experiment identity
	// recorded) but sees the parameter default.
	if control.ResolvedValue != "blue" {
		t.Errorf("control value = %v, want blue", control.ResolvedValue)
	}
	if control.ExposableType == nil || *control.ExposableType != "Experiment" || *control.ExposableID != 20 {
		t.Errorf("control exposable = %+v, want Experiment 20", control)
	}
	if control.AudienceID == nil || *control.AudienceID != 30 {
		t.Errorf("control audience = %v, want 30", control.AudienceID)
	}
	if control.UserID == nil || *control.UserID != controlUser {
		t.Errorf("control user_id = %v, want %s", control.UserID, controlUser)
	}

	treatment, err := resolver.exposureFor(User{UserID: treatmentUser}, "button-color")
	if err != nil {
		t.Fatalf("treatment resolve: %v", err)
	}
	if treatment.ResolvedValue != "green" {
		t.Errorf("treatment value = %v, want green", treatment.ResolvedValue)
	}
	if treatment.AudienceID == nil || *treatment.AudienceID != 31 {
		t.Errorf("treatment audience = %v, want 31", treatment.AudienceID)
	}
}

func TestResolveIsConsistentPerUser(t *testing.T) {
	resolver := mustResolver(t, testConfigJSON)
	user := User{UserID: "consistent-user"}
	first, err := resolver.exposureFor(user, "button-color")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := resolver.exposureFor(user, "button-color")
		if err != nil {
			t.Fatal(err)
		}
		if again.ResolvedValue != first.ResolvedValue {
			t.Fatalf("resolution changed between calls: %v then %v", first.ResolvedValue, again.ResolvedValue)
		}
	}
}

func TestDefaultPathWhenNothingControlsParameter(t *testing.T) {
	resolver := mustResolver(t, testConfigJSON)
	// `discount` is controlled by nothing: default path, no identity fields.
	exposure, err := resolver.exposureFor(User{UserID: "u1"}, "discount")
	if err != nil {
		t.Fatal(err)
	}
	if exposure.ResolvedValue != 0.1 {
		t.Errorf("default value = %v, want 0.1 (cast Float)", exposure.ResolvedValue)
	}
	if exposure.UserID != nil || exposure.ExposableType != nil || exposure.ExposableID != nil || exposure.AudienceID != nil {
		t.Errorf("default path must carry nil identity fields, got %+v", exposure)
	}
	if exposure.ParameterID != 11 || exposure.SpaceID != 1 {
		t.Errorf("default path ids = %d/%d, want 11/1", exposure.ParameterID, exposure.SpaceID)
	}
}

func TestFeatureFlagWinsOverExperiment(t *testing.T) {
	resolver := mustResolver(t, featureFlagConfigJSON)
	// vip_1 is in the flag's user list AND inside the experiment's 100%
	// allocation — FF-first ordering must win.
	exposure, err := resolver.exposureFor(User{UserID: "vip_1"}, "button-color")
	if err != nil {
		t.Fatal(err)
	}
	if exposure.ResolvedValue != "gold" {
		t.Errorf("value = %v, want gold (feature flag first)", exposure.ResolvedValue)
	}
	if *exposure.ExposableType != "FeatureFlag" || *exposure.ExposableID != 50 {
		t.Errorf("exposable = %s/%d, want FeatureFlag/50", *exposure.ExposableType, *exposure.ExposableID)
	}

	// A non-VIP user falls through to the experiment.
	other, err := resolver.exposureFor(User{UserID: "someone-else"}, "button-color")
	if err != nil {
		t.Fatal(err)
	}
	if *other.ExposableType != "Experiment" {
		t.Errorf("non-VIP exposable = %s, want Experiment", *other.ExposableType)
	}
}

func TestExplicitFalsyOverrideWins(t *testing.T) {
	resolver := mustResolver(t, featureFlagConfigJSON)
	// new-checkout defaults to true; the predicate flag sets it to an
	// explicit false. Falsy override must win — a truthiness check would
	// wrongly fall through to the default.
	exposure, err := resolver.exposureFor(User{UserID: "u1", Email: "dev@abmeter.com"}, "new-checkout")
	if err != nil {
		t.Fatal(err)
	}
	if exposure.ResolvedValue != false {
		t.Errorf("value = %v, want explicit false override", exposure.ResolvedValue)
	}
}

func TestMissingEmailIsSilentNoMatchForPredicate(t *testing.T) {
	resolver := mustResolver(t, featureFlagConfigJSON)
	exposure, err := resolver.exposureFor(User{UserID: "u1"}, "new-checkout")
	if err != nil {
		t.Fatalf("missing email must not error: %v", err)
	}
	if exposure.ResolvedValue != true {
		t.Errorf("value = %v, want default true (predicate no-match)", exposure.ResolvedValue)
	}
	if exposure.ExposableType != nil {
		t.Errorf("expected default path, got exposable %v", *exposure.ExposableType)
	}
}

func TestUnknownParameterErrors(t *testing.T) {
	resolver := mustResolver(t, testConfigJSON)
	_, err := resolver.exposureFor(User{UserID: "u1"}, "nope")
	if err == nil || !strings.Contains(err.Error(), `"nope" not found`) {
		t.Errorf("expected parameter-not-found error, got %v", err)
	}
}

func TestMissingUserIDErrors(t *testing.T) {
	resolver := mustResolver(t, testConfigJSON)
	if _, err := resolver.exposureFor(User{}, "button-color"); err == nil {
		t.Error("expected error for empty user_id")
	}
}

func TestSaltIsRequired(t *testing.T) {
	saltless := strings.Replace(testConfigJSON, `"salt": "experiment-salt-20",`, "", 1)
	if _, err := newResolver([]byte(saltless)); err == nil || !strings.Contains(err.Error(), "salt") {
		t.Errorf("expected missing-salt error, got %v", err)
	}
}

func TestUnknownSpaceErrors(t *testing.T) {
	broken := strings.Replace(testConfigJSON, `"space_id": 1,
      "salt"`, `"space_id": 99,
      "salt"`, 1)
	if _, err := newResolver([]byte(broken)); err == nil || !strings.Contains(err.Error(), "99") {
		t.Errorf("expected unknown-space error, got %v", err)
	}
}

func TestBooleanAndIntegerDefaultCasts(t *testing.T) {
	resolver := mustResolver(t, `{
	  "spaces": [{"id": 1, "salt": "s"}],
	  "parameters": [
	    {"id": 1, "slug": "flag", "parameter_type": "Boolean", "default_value": "true", "space_id": 1},
	    {"id": 2, "slug": "limit", "parameter_type": "Integer", "default_value": "42", "space_id": 1}
	  ],
	  "feature_flags": [],
	  "experiments": []
	}`)
	flag, _ := resolver.exposureFor(User{UserID: "u"}, "flag")
	if flag.ResolvedValue != true {
		t.Errorf("Boolean default = %v (%T), want true", flag.ResolvedValue, flag.ResolvedValue)
	}
	limit, _ := resolver.exposureFor(User{UserID: "u"}, "limit")
	if limit.ResolvedValue != int64(42) {
		t.Errorf("Integer default = %v (%T), want int64(42)", limit.ResolvedValue, limit.ResolvedValue)
	}
}

func TestAllocationSplitIsRoughlyEven(t *testing.T) {
	resolver := mustResolver(t, testConfigJSON)
	treatment := 0
	const n = 2000
	for i := 0; i < n; i++ {
		exposure, err := resolver.exposureFor(User{UserID: "bulk_user_" + itoa(i)}, "button-color")
		if err != nil {
			t.Fatal(err)
		}
		if exposure.ResolvedValue == "green" {
			treatment++
		}
	}
	// 50/50 split; allow generous slack (binomial sd ~1.1% at n=2000).
	if treatment < n*40/100 || treatment > n*60/100 {
		t.Errorf("treatment share %d/%d far from 50/50", treatment, n)
	}
}
