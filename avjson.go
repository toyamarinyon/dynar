package dynar

import (
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// avToWire converts an AttributeValue to the DynamoDB JSON wire shape,
// e.g. {"S":"hello"} / {"N":"1.5"} / {"M":{...}}.
func avToWire(av types.AttributeValue) any {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		return map[string]any{"S": v.Value}
	case *types.AttributeValueMemberN:
		return map[string]any{"N": v.Value}
	case *types.AttributeValueMemberB:
		return map[string]any{"B": base64.StdEncoding.EncodeToString(v.Value)}
	case *types.AttributeValueMemberBOOL:
		return map[string]any{"BOOL": v.Value}
	case *types.AttributeValueMemberNULL:
		return map[string]any{"NULL": v.Value}
	case *types.AttributeValueMemberSS:
		return map[string]any{"SS": v.Value}
	case *types.AttributeValueMemberNS:
		return map[string]any{"NS": v.Value}
	case *types.AttributeValueMemberBS:
		out := make([]string, len(v.Value))
		for i, b := range v.Value {
			out[i] = base64.StdEncoding.EncodeToString(b)
		}
		return map[string]any{"BS": out}
	case *types.AttributeValueMemberL:
		out := make([]any, len(v.Value))
		for i, e := range v.Value {
			out[i] = avToWire(e)
		}
		return map[string]any{"L": out}
	case *types.AttributeValueMemberM:
		out := make(map[string]any, len(v.Value))
		for k, e := range v.Value {
			out[k] = avToWire(e)
		}
		return map[string]any{"M": out}
	default:
		return map[string]any{"NULL": true}
	}
}

// itemToWire converts a whole item.
func itemToWire(item map[string]types.AttributeValue) map[string]any {
	out := make(map[string]any, len(item))
	for k, v := range item {
		out[k] = avToWire(v)
	}
	return out
}

// wireToAV converts one decoded JSON value (a single-member object like
// {"S":"x"}) back to a typed AttributeValue.
func wireToAV(v any) (types.AttributeValue, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, fmt.Errorf("invalid attribute value")
	}
	for tag, raw := range m {
		switch tag {
		case "S":
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("invalid S attribute")
			}
			return &types.AttributeValueMemberS{Value: s}, nil
		case "N":
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("invalid N attribute")
			}
			return &types.AttributeValueMemberN{Value: s}, nil
		case "B":
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("invalid B attribute")
			}
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, fmt.Errorf("invalid B attribute: %w", err)
			}
			return &types.AttributeValueMemberB{Value: b}, nil
		case "BOOL":
			b, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("invalid BOOL attribute")
			}
			return &types.AttributeValueMemberBOOL{Value: b}, nil
		case "NULL":
			b, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("invalid NULL attribute")
			}
			return &types.AttributeValueMemberNULL{Value: b}, nil
		case "SS":
			l, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("invalid SS attribute")
			}
			out := make([]string, 0, len(l))
			for _, e := range l {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("invalid SS element")
				}
				out = append(out, s)
			}
			return &types.AttributeValueMemberSS{Value: out}, nil
		case "NS":
			l, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("invalid NS attribute")
			}
			out := make([]string, 0, len(l))
			for _, e := range l {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("invalid NS element")
				}
				out = append(out, s)
			}
			return &types.AttributeValueMemberNS{Value: out}, nil
		case "BS":
			l, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("invalid BS attribute")
			}
			out := make([][]byte, 0, len(l))
			for _, e := range l {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("invalid BS element")
				}
				b, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return nil, fmt.Errorf("invalid BS element: %w", err)
				}
				out = append(out, b)
			}
			return &types.AttributeValueMemberBS{Value: out}, nil
		case "L":
			l, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("invalid L attribute")
			}
			out := make([]types.AttributeValue, 0, len(l))
			for _, e := range l {
				av, err := wireToAV(e)
				if err != nil {
					return nil, err
				}
				out = append(out, av)
			}
			return &types.AttributeValueMemberL{Value: out}, nil
		case "M":
			mm, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid M attribute")
			}
			out := make(map[string]types.AttributeValue, len(mm))
			for k, e := range mm {
				av, err := wireToAV(e)
				if err != nil {
					return nil, err
				}
				out[k] = av
			}
			return &types.AttributeValueMemberM{Value: out}, nil
		default:
			return nil, fmt.Errorf("unknown attribute type %q", tag)
		}
	}
	return nil, fmt.Errorf("invalid attribute value")
}

// wireToItem converts {"attr":{"S":"v"},...} to a typed item.
func wireToItem(v any) (map[string]types.AttributeValue, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("item must be an object")
	}
	out := make(map[string]types.AttributeValue, len(m))
	for k, e := range m {
		av, err := wireToAV(e)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: %w", k, err)
		}
		out[k] = av
	}
	return out, nil
}

// validateItem enforces DynamoDB's attribute-level constraints.
func validateItem(item map[string]types.AttributeValue) error {
	for name, av := range item {
		if err := validateAV(av); err != nil {
			return fmt.Errorf("attribute %q: %w", name, err)
		}
	}
	return nil
}

func validateAV(av types.AttributeValue) error {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		return nil
	case *types.AttributeValueMemberN:
		if err := validateNumber(v.Value); err != nil {
			return err
		}
		return nil
	case *types.AttributeValueMemberB:
		return nil
	case *types.AttributeValueMemberBOOL:
		return nil
	case *types.AttributeValueMemberNULL:
		if !v.Value {
			return fmt.Errorf("NULL must be true")
		}
		return nil
	case *types.AttributeValueMemberSS:
		if len(v.Value) == 0 {
			return fmt.Errorf("an string set may not be empty")
		}
		return nil
	case *types.AttributeValueMemberNS:
		if len(v.Value) == 0 {
			return fmt.Errorf("an number set may not be empty")
		}
		for _, n := range v.Value {
			if err := validateNumber(n); err != nil {
				return err
			}
		}
		return nil
	case *types.AttributeValueMemberBS:
		if len(v.Value) == 0 {
			return fmt.Errorf("an binary set may not be empty")
		}
		return nil
	case *types.AttributeValueMemberL:
		for _, e := range v.Value {
			if err := validateAV(e); err != nil {
				return err
			}
		}
		return nil
	case *types.AttributeValueMemberM:
		for k, e := range v.Value {
			if err := validateAV(e); err != nil {
				return fmt.Errorf("attribute %q: %w", k, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported attribute value type %T", av)
	}
}

// itemSize approximates DynamoDB's item size accounting: the sum of
// attribute name lengths (UTF-8 bytes) and attribute value sizes. Raw
// binary length counts, not its base64 transport encoding.
func itemSize(item map[string]types.AttributeValue) int64 {
	var total int64
	for name, av := range item {
		total += int64(len(name)) + avSize(av)
	}
	return total
}

// avSize returns the value portion of an attribute's size, per
// DynamoDB's documented rules: L and M carry 3 bytes of overhead plus
// 1 byte per element; sets have no overhead beyond their elements.
func avSize(av types.AttributeValue) int64 {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		return int64(len(v.Value))
	case *types.AttributeValueMemberN:
		return numberSize(v.Value)
	case *types.AttributeValueMemberB:
		return int64(len(v.Value))
	case *types.AttributeValueMemberBOOL:
		return 1
	case *types.AttributeValueMemberNULL:
		return 1
	case *types.AttributeValueMemberSS:
		var n int64
		for _, e := range v.Value {
			n += int64(len(e))
		}
		return n
	case *types.AttributeValueMemberNS:
		var n int64
		for _, e := range v.Value {
			n += numberSize(e)
		}
		return n
	case *types.AttributeValueMemberBS:
		var n int64
		for _, e := range v.Value {
			n += int64(len(e))
		}
		return n
	case *types.AttributeValueMemberL:
		n := int64(3)
		for _, e := range v.Value {
			n += 1 + avSize(e)
		}
		return n
	case *types.AttributeValueMemberM:
		n := int64(3)
		for k, e := range v.Value {
			n += 1 + int64(len(k)) + avSize(e)
		}
		return n
	default:
		return 0
	}
}

// numberSize is (1 byte per two significant digits) + 1 byte.
// Leading and trailing zeros do not count; zero itself is 1 byte.
func numberSize(s string) int64 {
	_, digits, _, err := parseDecimal(s)
	if err != nil || digits == "" {
		return 1
	}
	return int64(len(digits)+1)/2 + 1
}

// validateNumber checks DynamoDB's number constraints: decimal syntax,
// at most 38 significant digits, and a bounded exponent range.
func validateNumber(s string) error {
	sign, digits, exp, err := parseDecimal(s)
	if err != nil {
		return fmt.Errorf("the parameter cannot be converted to a numeric value: %s", s)
	}
	if sign == 0 {
		return nil
	}
	if len(digits) > 38 {
		return fmt.Errorf("number has more than 38 digits of precision: %s", s)
	}
	if exp > 126 || exp < -130 {
		return fmt.Errorf("number out of range: %s", s)
	}
	return nil
}
