package abmeter

// Test fixtures shaped exactly like real GET /api/v1/assignment-config
// `config` payloads (see AssignmentConfigBuilder + the Ruby serializers):
// per-experiment `salt` is present and required, parameter default_value is a
// string, variant parameter_values is a [{slug, value}] list.

// testConfigJSON: one space, one string parameter, one 100%-allocated
// experiment split 50/50 control (variant null) / treatment.
const testConfigJSON = `{
  "spaces": [{"id": 1, "salt": "space-salt-1"}],
  "parameters": [
    {"id": 10, "slug": "button-color", "parameter_type": "String", "default_value": "blue", "space_id": 1},
    {"id": 11, "slug": "discount", "parameter_type": "Float", "default_value": "0.1", "space_id": 1},
    {"id": 12, "slug": "new-checkout", "parameter_type": "Boolean", "default_value": "false", "space_id": 1}
  ],
  "feature_flags": [],
  "experiments": [
    {
      "id": 20,
      "space_id": 1,
      "salt": "experiment-salt-20",
      "range": [1, 100],
      "audience_variants": [
        {"audience": {"id": 30, "type": "random", "range": [1, 50]}, "variant": null},
        {
          "audience": {"id": 31, "type": "random", "range": [51, 100]},
          "variant": {"id": 40, "parameter_values": [{"slug": "button-color", "value": "green"}]}
        }
      ]
    }
  ]
}`

// featureFlagConfigJSON: a user_list flag and an email-predicate flag over
// the same parameters, plus a falsy-override flag proving an explicit false
// beats the parameter default.
const featureFlagConfigJSON = `{
  "spaces": [{"id": 1, "salt": "space-salt-1"}],
  "parameters": [
    {"id": 10, "slug": "button-color", "parameter_type": "String", "default_value": "blue", "space_id": 1},
    {"id": 12, "slug": "new-checkout", "parameter_type": "Boolean", "default_value": "true", "space_id": 1}
  ],
  "feature_flags": [
    {
      "id": 50,
      "audience": {"id": 60, "type": "user_list", "user_ids": ["vip_1", "vip_2"]},
      "variant": {"id": 70, "parameter_values": [{"slug": "button-color", "value": "gold"}]}
    },
    {
      "id": 51,
      "audience": {"id": 61, "type": "predicate", "predicate": "@abmeter\\.com$"},
      "variant": {"id": 71, "parameter_values": [{"slug": "new-checkout", "value": false}]}
    }
  ],
  "experiments": [
    {
      "id": 20,
      "space_id": 1,
      "salt": "experiment-salt-20",
      "range": [1, 100],
      "audience_variants": [
        {
          "audience": {"id": 31, "type": "random", "range": [1, 100]},
          "variant": {"id": 40, "parameter_values": [{"slug": "button-color", "value": "green"}]}
        }
      ]
    }
  ]
}`
