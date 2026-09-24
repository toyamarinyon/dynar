package dynar

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Key attribute values are encoded into a BLOB whose byte order matches
// DynamoDB's sort order, so SQL ORDER BY reproduces Query ordering.
//
//	S: 0x01 + utf8 bytes (0x00 escaped as 0x00 0xFF) + 0x00 0x00
//	N: 0x02 + sign byte + order-preserving decimal encoding
//	B: 0x03 + raw bytes  (0x00 escaped as 0x00 0xFF) + 0x00 0x00
//
// For N the payload after the sign byte is the decimal encoding
// (2-byte biased exponent, then significand digits + 0x00 terminator);
// negative numbers have the payload bitwise-complemented so that byte
// order equals numeric order.

const (
	keyTagS = 0x01
	keyTagN = 0x02
	keyTagB = 0x03
)

func encodeKeyValue(av types.AttributeValue) ([]byte, error) {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		if v.Value == "" {
			return nil, fmt.Errorf("key attribute cannot contain an empty string value")
		}
		out := []byte{keyTagS}
		out = appendEscaped(out, []byte(v.Value))
		return append(out, 0x00, 0x00), nil
	case *types.AttributeValueMemberB:
		if len(v.Value) == 0 {
			return nil, fmt.Errorf("key attribute cannot contain an empty binary value")
		}
		out := []byte{keyTagB}
		out = appendEscaped(out, v.Value)
		return append(out, 0x00, 0x00), nil
	case *types.AttributeValueMemberN:
		return encodeNumberKey(v.Value)
	default:
		return nil, fmt.Errorf("invalid key attribute type %T (must be S, N, or B)", av)
	}
}

// appendEscaped appends src with every 0x00 byte replaced by 0x00 0xFF,
// so a bare 0x00 can act as a terminator.
func appendEscaped(dst, src []byte) []byte {
	for _, b := range src {
		if b == 0x00 {
			dst = append(dst, 0x00, 0xFF)
		} else {
			dst = append(dst, b)
		}
	}
	return dst
}

// parseDecimal normalizes a DynamoDB number string without losing
// precision. It returns sign (-1, 0, 1), the significand digits (no
// leading or trailing zeros), and exp such that
//
//	value = sign * 0.digits * 10^exp     (0.1 <= 0.digits < 1)
func parseDecimal(s string) (sign int, digits string, exp int, err error) {
	if s == "" {
		return 0, "", 0, fmt.Errorf("empty number")
	}
	sign = 1
	i := 0
	if s[0] == '+' || s[0] == '-' {
		if s[0] == '-' {
			sign = -1
		}
		i++
	}
	rest := s[i:]

	mant := rest
	expStr := ""
	if j := strings.IndexAny(rest, "eE"); j >= 0 {
		mant = rest[:j]
		expStr = rest[j+1:]
	}
	e := 0
	if expStr != "" {
		var n int
		if _, err := fmt.Sscanf(expStr, "%d", &n); err != nil {
			return 0, "", 0, fmt.Errorf("invalid exponent %q", expStr)
		}
		// guard against absurd exponents
		if n > 100000 || n < -100000 {
			return 0, "", 0, fmt.Errorf("exponent out of range %q", expStr)
		}
		e = n
	}

	intPart := mant
	fracPart := ""
	if j := strings.IndexByte(mant, '.'); j >= 0 {
		intPart = mant[:j]
		fracPart = mant[j+1:]
	}
	if intPart == "" && fracPart == "" {
		return 0, "", 0, fmt.Errorf("invalid number %q", s)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, "", 0, fmt.Errorf("invalid number %q", s)
		}
	}
	if intPart == "" && fracPart == "" {
		return 0, "", 0, fmt.Errorf("invalid number %q", s)
	}

	raw := intPart + fracPart
	scale := len(fracPart)

	// value = raw * 10^(e - scale); normalize to 0.digits * 10^E
	trimmed := strings.TrimLeft(raw, "0")
	lz := len(raw) - len(trimmed)
	trimmed = strings.TrimRight(trimmed, "0")
	if trimmed == "" {
		return 0, "", 0, nil // zero
	}
	E := e - scale + len(raw) - lz
	return sign, trimmed, E, nil
}

// encodeNumberKey encodes an N attribute as order-preserving bytes.
func encodeNumberKey(s string) ([]byte, error) {
	sign, digits, exp, err := parseDecimal(s)
	if err != nil {
		return nil, err
	}
	out := []byte{keyTagN}
	switch {
	case sign == 0:
		out = append(out, 0x01)
	case sign > 0:
		if exp < -32768 || exp > 32767 {
			return nil, fmt.Errorf("number exponent out of range: %s", s)
		}
		out = append(out, 0x02, byte(uint16(exp+32768)>>8), byte(uint16(exp+32768)))
		out = append(out, digits...)
		out = append(out, 0x00)
	default:
		if exp < -32768 || exp > 32767 {
			return nil, fmt.Errorf("number exponent out of range: %s", s)
		}
		out = append(out, 0x00, ^byte(uint16(exp+32768)>>8), ^byte(uint16(exp+32768)))
		for i := 0; i < len(digits); i++ {
			out = append(out, ^digits[i])
		}
		out = append(out, 0xFF) // complement of the 0x00 terminator
	}
	return out, nil
}

// keyPrefix encodes a value for begins_with: the encoding of the value
// without its terminator. Range [prefix, successor(prefix)) covers all
// encodings that start with prefix.
func keyPrefix(av types.AttributeValue) ([]byte, error) {
	full, err := encodeKeyValue(av)
	if err != nil {
		return nil, err
	}
	switch av.(type) {
	case *types.AttributeValueMemberS, *types.AttributeValueMemberB:
		// strip the 0x00 0x00 terminator
		return full[:len(full)-2], nil
	default:
		return nil, fmt.Errorf("begins_with is only supported for S and B key types")
	}
}

// successor returns the smallest byte string strictly greater than all
// strings having b as a prefix, or nil if there is none (all 0xFF).
func successor(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

// itemKey encodes the partition and (optional) sort key columns of an item.
func itemKey(t *tableMeta, item map[string]types.AttributeValue) (pk, sk []byte, err error) {
	hv, ok := item[t.hashKey]
	if !ok {
		return nil, nil, fmt.Errorf("missing the key %s in the item", t.hashKey)
	}
	if got := keyTypeOf(hv); got != t.hashType {
		return nil, nil, fmt.Errorf("type mismatch for key %s expected: %s actual: %s", t.hashKey, t.hashType, got)
	}
	pk, err = encodeKeyValue(hv)
	if err != nil {
		return nil, nil, err
	}
	if t.rangeKey == "" {
		return pk, nil, nil
	}
	sv, ok := item[t.rangeKey]
	if !ok {
		return nil, nil, fmt.Errorf("missing the key %s in the item", t.rangeKey)
	}
	if got := keyTypeOf(sv); got != t.rangeType {
		return nil, nil, fmt.Errorf("type mismatch for key %s expected: %s actual: %s", t.rangeKey, t.rangeType, got)
	}
	sk, err = encodeKeyValue(sv)
	if err != nil {
		return nil, nil, err
	}
	return pk, sk, nil
}

// keyTypeOf reports "S", "N", or "B" for scalar key-able values.
func keyTypeOf(av types.AttributeValue) string {
	switch av.(type) {
	case *types.AttributeValueMemberS:
		return "S"
	case *types.AttributeValueMemberN:
		return "N"
	case *types.AttributeValueMemberB:
		return "B"
	default:
		return ""
	}
}

// compareNumbers compares two DynamoDB number strings numerically.
// Returns -1, 0, or 1.
func compareNumbers(a, b string) (int, error) {
	as, ad, ae, err := parseDecimal(a)
	if err != nil {
		return 0, err
	}
	bs, bd, be, err := parseDecimal(b)
	if err != nil {
		return 0, err
	}
	if as != bs {
		if as < bs {
			return -1, nil
		}
		return 1, nil
	}
	if as == 0 {
		return 0, nil
	}
	cmp := 0
	switch {
	case ae != be:
		if ae < be {
			cmp = -1
		} else {
			cmp = 1
		}
	default:
		cmp = strings.Compare(padRight(ad, bd), padRight(bd, ad))
	}
	return cmp * as, nil
}

// padRight pads the shorter digit string with trailing zeros so two
// significands compare lexicographically.
func padRight(d, other string) string {
	for len(d) < len(other) {
		d += "0"
	}
	return d
}

// numbersEqual reports whether two N strings are numerically equal.
func numbersEqual(a, b string) bool {
	c, err := compareNumbers(a, b)
	return err == nil && c == 0
}

// keyEqual compares two encoded keys.
func keyEqual(a, b []byte) bool { return bytes.Equal(a, b) }
