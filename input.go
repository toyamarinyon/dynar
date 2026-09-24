package dynar

import (
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Helpers to pull typed fields out of the decoded request JSON.
// Every accessor returns a *apiError (ValidationException) on bad input.

func reqString(in map[string]any, field string) (string, *apiError) {
	v, ok := in[field]
	if !ok {
		return "", errValidation("missing required parameter %s", field)
	}
	s, ok := v.(string)
	if !ok {
		return "", errValidation("parameter %s must be a string", field)
	}
	return s, nil
}

func optString(in map[string]any, field string) string {
	if v, ok := in[field].(string); ok {
		return v
	}
	return ""
}

func optInt(in map[string]any, field string) (int64, *apiError) {
	v, ok := in[field]
	if !ok {
		return 0, nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, errValidation("parameter %s must be a number", field)
	}
	i, err := n.Int64()
	if err != nil {
		return 0, errValidation("parameter %s must be an integer", field)
	}
	return i, nil
}

func optBool(in map[string]any, field string) bool {
	v, _ := in[field].(bool)
	return v
}

func reqItem(in map[string]any, field string) (map[string]types.AttributeValue, *apiError) {
	v, ok := in[field]
	if !ok {
		return nil, errValidation("missing required parameter %s", field)
	}
	item, err := wireToItem(v)
	if err != nil {
		return nil, errValidation("invalid %s: %v", field, err)
	}
	return item, nil
}

func optItem(in map[string]any, field string) (map[string]types.AttributeValue, *apiError) {
	v, ok := in[field]
	if !ok {
		return nil, nil
	}
	item, err := wireToItem(v)
	if err != nil {
		return nil, errValidation("invalid %s: %v", field, err)
	}
	return item, nil
}

func optNameMap(in map[string]any, field string) (map[string]string, *apiError) {
	v, ok := in[field]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errValidation("parameter %s must be an object", field)
	}
	out := make(map[string]string, len(m))
	for k, e := range m {
		s, ok := e.(string)
		if !ok {
			return nil, errValidation("parameter %s values must be strings", field)
		}
		out[k] = s
	}
	return out, nil
}

// rejectLegacy fails the request if any legacy (pre-expression) parameter
// is present. These are not silently ignored.
func rejectLegacy(in map[string]any, fields ...string) *apiError {
	for _, f := range fields {
		if _, ok := in[f]; ok {
			return errValidation(
				"dynar: legacy parameter %s is not supported; use the expression-based parameters", f)
		}
	}
	return nil
}

// exprEnv carries the expression attribute names/values shared by every
// expression in one request.
type exprEnv struct {
	names  map[string]string
	values map[string]types.AttributeValue
}

func getExprEnv(in map[string]any) (*exprEnv, *apiError) {
	names, aerr := optNameMap(in, "ExpressionAttributeNames")
	if aerr != nil {
		return nil, aerr
	}
	vals, aerr := optItem(in, "ExpressionAttributeValues")
	if aerr != nil {
		return nil, aerr
	}
	for _, v := range vals {
		if err := validateAV(v); err != nil {
			return nil, errValidation("invalid ExpressionAttributeValues: %v", err)
		}
	}
	return &exprEnv{names: names, values: vals}, nil
}

// resolveName maps "#x" to its real attribute name.
func (e *exprEnv) resolveName(ref string) (string, error) {
	if e == nil || e.names == nil {
		return "", fmt.Errorf("expression attribute name %s used but not defined", ref)
	}
	n, ok := e.names[ref]
	if !ok {
		return "", fmt.Errorf("expression attribute name %s used but not defined", ref)
	}
	return n, nil
}

// resolveValue maps ":x" to its AttributeValue.
func (e *exprEnv) resolveValue(ref string) (types.AttributeValue, error) {
	if e == nil || e.values == nil {
		return nil, fmt.Errorf("expression attribute value %s used but not defined", ref)
	}
	v, ok := e.values[ref]
	if !ok {
		return nil, fmt.Errorf("expression attribute value %s used but not defined", ref)
	}
	return v, nil
}
