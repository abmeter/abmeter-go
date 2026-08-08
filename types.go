package abmeter

import (
	"fmt"
	"math"
	"strconv"
)

// Parameter type names as the platform emits them.
const (
	typeString  = "String"
	typeInteger = "Integer"
	typeFloat   = "Float"
	typeBoolean = "Boolean"
)

// castValue casts a wire value (the platform serializes default_value as a
// string) to the parameter's declared type. Only casting is needed on the
// client — the type-validation surface (registry, is-valid checks) is a
// server-side concern and intentionally absent, per the client-only core rule.
func castValue(value any, parameterType string) (any, error) {
	switch parameterType {
	case typeString:
		return castString(value), nil
	case typeInteger:
		return castInteger(value)
	case typeFloat:
		return castFloat(value)
	case typeBoolean:
		return castBoolean(value)
	default:
		// Unknown type: pass through unchanged.
		return value, nil
	}
}

func castString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", value)
}

func castInteger(value any) (int64, error) {
	switch v := value.(type) {
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot cast %q to Integer", v)
		}
		return n, nil
	case float64: // JSON numbers decode as float64
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("cannot cast %v to Integer", v)
		}
		return int64(v), nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	default:
		return 0, fmt.Errorf("cannot cast %v to Integer", value)
	}
}

func castFloat(value any) (float64, error) {
	switch v := value.(type) {
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot cast %q to Float", v)
		}
		return f, nil
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("cannot cast %v to Float", value)
	}
}

func castBoolean(value any) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case nil:
		return false, nil
	case string:
		switch v {
		case "true", "TRUE", "t", "T", "1":
			return true, nil
		case "false", "FALSE", "f", "F", "0", "":
			return false, nil
		}
	case float64:
		switch v {
		case 1:
			return true, nil
		case 0:
			return false, nil
		}
	}
	return false, fmt.Errorf("cannot cast %v to Boolean", value)
}
