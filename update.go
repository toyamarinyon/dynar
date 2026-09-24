package dynar

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// UpdateExpression:
//
//	update  := section+
//	section := SET assign (',' assign)* | REMOVE path (',' path)*
//	         | ADD path operand (',' path operand)*
//	         | DELETE path operand (',' path operand)*
//	assign  := path '=' uval
//	uval    := operand | operand '+' operand | operand '-' operand
//	         | if_not_exists '(' path ',' operand ')'
//	         | list_append '(' operand ',' operand ')'

type updateOp int

const (
	updSet updateOp = iota
	updRemove
	updAdd
	updDelete
)

type setValueKind int

const (
	setPlain setValueKind = iota
	setAdd                // rvalue + operand
	setSub                // rvalue - operand
)

// rvalue is a right-hand-side value in SET: an operand or a nested
// if_not_exists / list_append call.
type rvalue struct {
	opnd operand
	fn   string // "", "if_not_exists", "list_append"
	a, b *rvalue
}

type setValue struct {
	kind setValueKind
	a    rvalue
	b    operand
}

type updateAction struct {
	op    updateOp
	path  []pathElem
	value operand // ADD / DELETE
	set   setValue
}

func parseUpdateExpr(s string, env *exprEnv) ([]updateAction, error) {
	toks, err := lexExpr(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, env: env}
	var actions []updateAction
	for !p.atEnd() {
		switch {
		case p.takeKeyword("SET"):
			for {
				a, err := p.parseSetAction()
				if err != nil {
					return nil, err
				}
				actions = append(actions, a)
				if p.peek().kind == tkComma {
					p.next()
					continue
				}
				break
			}
		case p.takeKeyword("REMOVE"):
			for {
				path, err := p.parsePath()
				if err != nil {
					return nil, err
				}
				actions = append(actions, updateAction{op: updRemove, path: path})
				if p.peek().kind == tkComma {
					p.next()
					continue
				}
				break
			}
		case p.takeKeyword("ADD"):
			for {
				path, err := p.parsePath()
				if err != nil {
					return nil, err
				}
				v, err := p.parseOperand()
				if err != nil {
					return nil, err
				}
				actions = append(actions, updateAction{op: updAdd, path: path, value: v})
				if p.peek().kind == tkComma {
					p.next()
					continue
				}
				break
			}
		case p.takeKeyword("DELETE"):
			for {
				path, err := p.parsePath()
				if err != nil {
					return nil, err
				}
				v, err := p.parseOperand()
				if err != nil {
					return nil, err
				}
				actions = append(actions, updateAction{op: updDelete, path: path, value: v})
				if p.peek().kind == tkComma {
					p.next()
					continue
				}
				break
			}
		default:
			return nil, fmt.Errorf("expected SET, REMOVE, ADD, or DELETE, found %q", p.peek().text)
		}
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("empty update expression")
	}
	// DynamoDB rejects overlapping document paths in one expression.
	for i := range actions {
		for j := i + 1; j < len(actions); j++ {
			if pathsOverlap(actions[i].path, actions[j].path) {
				return nil, fmt.Errorf(
					"Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [%s], path two: [%s]",
					renderPath(actions[i].path), renderPath(actions[j].path))
			}
		}
	}
	return actions, nil
}

// pathsOverlap reports whether one path is a prefix of (or equal to) the
// other — e.g. a vs a.b, or a[0] vs a[0].x. Sibling paths do not overlap.
func pathsOverlap(a, b []pathElem) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].isIdx != b[i].isIdx || a[i].name != b[i].name || a[i].index != b[i].index {
			return false
		}
	}
	return true
}

func (p *parser) parseSetAction() (updateAction, error) {
	path, err := p.parsePath()
	if err != nil {
		return updateAction{}, err
	}
	if err := p.expect(tkOp, "'='"); err != nil {
		return updateAction{}, err
	}
	if p.toks[p.pos-1].text != "=" {
		return updateAction{}, fmt.Errorf("expected '=' in SET")
	}
	v, err := p.parseSetValue()
	if err != nil {
		return updateAction{}, err
	}
	return updateAction{op: updSet, path: path, set: v}, nil
}

func (p *parser) parseSetValue() (setValue, error) {
	a, err := p.parseRvalue()
	if err != nil {
		return setValue{}, err
	}
	switch p.peek().kind {
	case tkPlus:
		p.next()
		b, err := p.parseOperand()
		if err != nil {
			return setValue{}, err
		}
		return setValue{kind: setAdd, a: a, b: b}, nil
	case tkMinus:
		p.next()
		b, err := p.parseOperand()
		if err != nil {
			return setValue{}, err
		}
		return setValue{kind: setSub, a: a, b: b}, nil
	}
	return setValue{kind: setPlain, a: a}, nil
}

// parseRvalue reads an operand or a nested if_not_exists / list_append call.
func (p *parser) parseRvalue() (rvalue, error) {
	t := p.peek()
	if t.kind == tkIdent {
		lc := strings.ToLower(t.text)
		if (lc == "if_not_exists" || lc == "list_append") && p.toks[p.pos+1].kind == tkLParen {
			p.next()
			p.next() // '('
			a, err := p.parseRvalue()
			if err != nil {
				return rvalue{}, err
			}
			if lc == "if_not_exists" && (a.fn != "" || a.opnd.kind != opndPath) {
				return rvalue{}, fmt.Errorf("if_not_exists requires an attribute path as first argument")
			}
			if err := p.expect(tkComma, "','"); err != nil {
				return rvalue{}, err
			}
			b, err := p.parseRvalue()
			if err != nil {
				return rvalue{}, err
			}
			if err := p.expect(tkRParen, "')'"); err != nil {
				return rvalue{}, err
			}
			return rvalue{fn: lc, a: &a, b: &b}, nil
		}
	}
	o, err := p.parseOperand()
	if err != nil {
		return rvalue{}, err
	}
	return rvalue{opnd: o}, nil
}

// applyUpdate executes parsed update actions against an item and returns
// the new item plus the set of top-level attributes that were touched
// (for UPDATED_OLD / UPDATED_NEW).
func applyUpdate(item map[string]types.AttributeValue, actions []updateAction, env *exprEnv) (map[string]types.AttributeValue, map[string]bool, error) {
	out := cloneItem(item)
	updated := map[string]bool{}

	// REMOVE with list indexes must be applied highest-index-first per list.
	var removes []updateAction
	for _, a := range actions {
		if a.op == updRemove {
			removes = append(removes, a)
			continue
		}
		var err error
		switch a.op {
		case updSet:
			err = applySet(out, a, env)
		case updAdd:
			err = applyAdd(out, a, env)
		case updDelete:
			err = applyDelete(out, a, env)
		}
		if err != nil {
			return nil, nil, err
		}
		updated[a.path[0].name] = true
	}
	// order index-removals descending within the same parent path
	sort.SliceStable(removes, func(i, j int) bool {
		pi, pj := removes[i].path, removes[j].path
		li, lj := pi[len(pi)-1], pj[len(pj)-1]
		if li.isIdx && lj.isIdx && sameParentPath(pi, pj) {
			return li.index > lj.index
		}
		return false
	})
	for _, a := range removes {
		if err := applyRemove(out, a.path); err != nil {
			return nil, nil, err
		}
		updated[a.path[0].name] = true
	}
	return out, updated, nil
}

func sameParentPath(a, b []pathElem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a)-1; i++ {
		if a[i].isIdx != b[i].isIdx || a[i].name != b[i].name || a[i].index != b[i].index {
			return false
		}
	}
	return true
}

func evalSetValue(sv setValue, item map[string]types.AttributeValue, env *exprEnv) (types.AttributeValue, error) {
	switch sv.kind {
	case setAdd, setSub:
		a, err := evalRvalue(sv.a, item, env)
		if err != nil {
			return nil, err
		}
		b, err := operandValue(sv.b, item, env)
		if err != nil {
			return nil, err
		}
		na, ok := a.(*types.AttributeValueMemberN)
		if !ok {
			return nil, fmt.Errorf("operand for +/- must be a number")
		}
		nb, ok := b.(*types.AttributeValueMemberN)
		if !ok {
			return nil, fmt.Errorf("operand for +/- must be a number")
		}
		return addNumbers(na.Value, nb.Value, sv.kind == setSub)
	default:
		return evalRvalue(sv.a, item, env)
	}
}

// evalRvalue evaluates a SET right-hand side. Missing paths are errors,
// except inside if_not_exists.
func evalRvalue(r rvalue, item map[string]types.AttributeValue, env *exprEnv) (types.AttributeValue, error) {
	switch r.fn {
	case "":
		return operandValue(r.opnd, item, env)
	case "if_not_exists":
		if v, ok := resolvePath(item, r.a.opnd.path); ok {
			return v, nil
		}
		return evalRvalue(*r.b, item, env)
	case "list_append":
		a, err := evalRvalue(*r.a, item, env)
		if err != nil {
			return nil, err
		}
		b, err := evalRvalue(*r.b, item, env)
		if err != nil {
			return nil, err
		}
		la, ok := a.(*types.AttributeValueMemberL)
		if !ok {
			return nil, fmt.Errorf("list_append first argument is not a list")
		}
		lb, ok := b.(*types.AttributeValueMemberL)
		if !ok {
			return nil, fmt.Errorf("list_append second argument is not a list")
		}
		out := make([]types.AttributeValue, 0, len(la.Value)+len(lb.Value))
		out = append(out, la.Value...)
		out = append(out, lb.Value...)
		return &types.AttributeValueMemberL{Value: out}, nil
	default:
		return nil, fmt.Errorf("unsupported function %s", r.fn)
	}
}

// operandValue resolves an operand used as an rvalue. Document paths that
// do not resolve are an error (unlike in conditions).
func operandValue(o operand, item map[string]types.AttributeValue, env *exprEnv) (types.AttributeValue, error) {
	v, ok, err := resolveOperand(o, item, env)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("attribute path does not exist: %s", renderPath(o.path))
	}
	return v, nil
}

func renderPath(path []pathElem) string {
	var b strings.Builder
	for i, e := range path {
		if e.isIdx {
			fmt.Fprintf(&b, "[%d]", e.index)
		} else {
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(e.name)
		}
	}
	return b.String()
}

func applySet(item map[string]types.AttributeValue, a updateAction, env *exprEnv) error {
	v, err := evalSetValue(a.set, item, env)
	if err != nil {
		return err
	}
	return setPath(item, a.path, v)
}

// setPath assigns v at path, creating intermediate M parents.
func setPath(item map[string]types.AttributeValue, path []pathElem, v types.AttributeValue) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}
	cur := item
	var curAV types.AttributeValue
	for i, e := range path {
		last := i == len(path)-1
		if e.isIdx {
			l, ok := curAV.(*types.AttributeValueMemberL)
			if !ok {
				return fmt.Errorf("path %s does not resolve to a list", renderPath(path[:i+1]))
			}
			if e.index >= len(l.Value) {
				return fmt.Errorf("list index %d out of range", e.index)
			}
			if last {
				l.Value[e.index] = v
				return nil
			}
			curAV = l.Value[e.index]
			continue
		}
		var m map[string]types.AttributeValue
		if i == 0 {
			m = item
		} else {
			mv, ok := curAV.(*types.AttributeValueMemberM)
			if !ok {
				return fmt.Errorf("path %s does not resolve to a map", renderPath(path[:i+1]))
			}
			m = mv.Value
		}
		if last {
			m[e.name] = v
			return nil
		}
		next, ok := m[e.name]
		if !ok {
			// auto-create intermediate map
			next = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}}
			m[e.name] = next
		}
		curAV = next
		cur = m
	}
	_ = cur
	return nil
}

func applyRemove(item map[string]types.AttributeValue, path []pathElem) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}
	// resolve the parent
	parentPath := path[:len(path)-1]
	last := path[len(path)-1]
	if len(parentPath) == 0 {
		if last.isIdx {
			return fmt.Errorf("invalid remove path")
		}
		delete(item, last.name)
		return nil
	}
	parent, ok := resolvePath(item, parentPath)
	if !ok {
		// removing a non-existent nested path is a no-op in DynamoDB
		return nil
	}
	if last.isIdx {
		l, ok := parent.(*types.AttributeValueMemberL)
		if !ok {
			return fmt.Errorf("path %s does not resolve to a list", renderPath(parentPath))
		}
		if last.index >= len(l.Value) {
			return fmt.Errorf("list index %d out of range", last.index)
		}
		l.Value = append(l.Value[:last.index], l.Value[last.index+1:]...)
		return nil
	}
	m, ok := parent.(*types.AttributeValueMemberM)
	if !ok {
		return fmt.Errorf("path %s does not resolve to a map", renderPath(parentPath))
	}
	delete(m.Value, last.name)
	return nil
}

func applyAdd(item map[string]types.AttributeValue, a updateAction, env *exprEnv) error {
	v, err := operandValue(a.value, item, env)
	if err != nil {
		return err
	}
	cur, exists := resolvePath(item, a.path)
	switch av := v.(type) {
	case *types.AttributeValueMemberN:
		if !exists {
			return setPath(item, a.path, v)
		}
		cn, ok := cur.(*types.AttributeValueMemberN)
		if !ok {
			return fmt.Errorf("ADD target is not a number")
		}
		sum, err := addNumbers(cn.Value, av.Value, false)
		if err != nil {
			return err
		}
		return setPath(item, a.path, sum)
	case *types.AttributeValueMemberSS:
		if !exists {
			return setPath(item, a.path, v)
		}
		cs, ok := cur.(*types.AttributeValueMemberSS)
		if !ok {
			return fmt.Errorf("ADD target is not a string set")
		}
		return setPath(item, a.path, &types.AttributeValueMemberSS{Value: unionStrings(cs.Value, av.Value)})
	case *types.AttributeValueMemberNS:
		if !exists {
			return setPath(item, a.path, v)
		}
		cs, ok := cur.(*types.AttributeValueMemberNS)
		if !ok {
			return fmt.Errorf("ADD target is not a number set")
		}
		return setPath(item, a.path, &types.AttributeValueMemberNS{Value: unionNumbers(cs.Value, av.Value)})
	case *types.AttributeValueMemberBS:
		if !exists {
			return setPath(item, a.path, v)
		}
		cs, ok := cur.(*types.AttributeValueMemberBS)
		if !ok {
			return fmt.Errorf("ADD target is not a binary set")
		}
		return setPath(item, a.path, &types.AttributeValueMemberBS{Value: unionBinaries(cs.Value, av.Value)})
	default:
		return fmt.Errorf("ADD only supports numbers and sets")
	}
}

func applyDelete(item map[string]types.AttributeValue, a updateAction, env *exprEnv) error {
	v, err := operandValue(a.value, item, env)
	if err != nil {
		return err
	}
	cur, exists := resolvePath(item, a.path)
	if !exists {
		// DELETE on a missing attribute is a no-op
		return nil
	}
	switch av := v.(type) {
	case *types.AttributeValueMemberSS:
		cs, ok := cur.(*types.AttributeValueMemberSS)
		if !ok {
			return fmt.Errorf("DELETE target is not a string set")
		}
		out := diffStrings(cs.Value, av.Value)
		return setOrRemoveEmptySet(item, a.path, out, func(s []string) types.AttributeValue {
			return &types.AttributeValueMemberSS{Value: s}
		})
	case *types.AttributeValueMemberNS:
		cs, ok := cur.(*types.AttributeValueMemberNS)
		if !ok {
			return fmt.Errorf("DELETE target is not a number set")
		}
		out := diffNumbers(cs.Value, av.Value)
		if len(out) == 0 {
			return applyRemove(item, a.path)
		}
		return setPath(item, a.path, &types.AttributeValueMemberNS{Value: out})
	case *types.AttributeValueMemberBS:
		cs, ok := cur.(*types.AttributeValueMemberBS)
		if !ok {
			return fmt.Errorf("DELETE target is not a binary set")
		}
		out := diffBinaries(cs.Value, av.Value)
		if len(out) == 0 {
			return applyRemove(item, a.path)
		}
		return setPath(item, a.path, &types.AttributeValueMemberBS{Value: out})
	default:
		return fmt.Errorf("DELETE only supports sets")
	}
}

func setOrRemoveEmptySet(item map[string]types.AttributeValue, path []pathElem, out []string, mk func([]string) types.AttributeValue) error {
	if len(out) == 0 {
		return applyRemove(item, path)
	}
	return setPath(item, path, mk(out))
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	out := append([]string{}, a...)
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}

func unionNumbers(a, b []string) []string {
	out := append([]string{}, a...)
	for _, s := range b {
		dup := false
		for _, e := range out {
			if numbersEqual(e, s) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}

func unionBinaries(a, b [][]byte) [][]byte {
	out := append([][]byte{}, a...)
	for _, s := range b {
		dup := false
		for _, e := range out {
			if bytes.Equal(e, s) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}

func diffStrings(a, b []string) []string {
	rm := map[string]bool{}
	for _, s := range b {
		rm[s] = true
	}
	var out []string
	for _, s := range a {
		if !rm[s] {
			out = append(out, s)
		}
	}
	return out
}

func diffNumbers(a, b []string) []string {
	var out []string
	for _, s := range a {
		keep := true
		for _, e := range b {
			if numbersEqual(s, e) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, s)
		}
	}
	return out
}

func diffBinaries(a, b [][]byte) [][]byte {
	var out [][]byte
	for _, s := range a {
		keep := true
		for _, e := range b {
			if bytes.Equal(s, e) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, s)
		}
	}
	return out
}

func cloneItem(item map[string]types.AttributeValue) map[string]types.AttributeValue {
	out := make(map[string]types.AttributeValue, len(item))
	for k, v := range item {
		out[k] = cloneAV(v)
	}
	return out
}

func cloneAV(v types.AttributeValue) types.AttributeValue {
	switch a := v.(type) {
	case *types.AttributeValueMemberS:
		return &types.AttributeValueMemberS{Value: a.Value}
	case *types.AttributeValueMemberN:
		return &types.AttributeValueMemberN{Value: a.Value}
	case *types.AttributeValueMemberB:
		return &types.AttributeValueMemberB{Value: append([]byte{}, a.Value...)}
	case *types.AttributeValueMemberBOOL:
		return &types.AttributeValueMemberBOOL{Value: a.Value}
	case *types.AttributeValueMemberNULL:
		return &types.AttributeValueMemberNULL{Value: a.Value}
	case *types.AttributeValueMemberSS:
		return &types.AttributeValueMemberSS{Value: append([]string{}, a.Value...)}
	case *types.AttributeValueMemberNS:
		return &types.AttributeValueMemberNS{Value: append([]string{}, a.Value...)}
	case *types.AttributeValueMemberBS:
		out := make([][]byte, len(a.Value))
		for i, b := range a.Value {
			out[i] = append([]byte{}, b...)
		}
		return &types.AttributeValueMemberBS{Value: out}
	case *types.AttributeValueMemberL:
		out := make([]types.AttributeValue, len(a.Value))
		for i, e := range a.Value {
			out[i] = cloneAV(e)
		}
		return &types.AttributeValueMemberL{Value: out}
	case *types.AttributeValueMemberM:
		out := make(map[string]types.AttributeValue, len(a.Value))
		for k, e := range a.Value {
			out[k] = cloneAV(e)
		}
		return &types.AttributeValueMemberM{Value: out}
	default:
		return v
	}
}

// addNumbers adds (or subtracts) two DynamoDB number strings exactly,
// returning a canonical decimal. It never passes through float64.
func addNumbers(a, b string, sub bool) (types.AttributeValue, error) {
	ra, ok := new(big.Rat).SetString(a)
	if !ok {
		return nil, fmt.Errorf("invalid number %q", a)
	}
	rb, ok := new(big.Rat).SetString(b)
	if !ok {
		return nil, fmt.Errorf("invalid number %q", b)
	}
	if sub {
		rb.Neg(rb)
	}
	return &types.AttributeValueMemberN{Value: ratDecimalString(ra.Add(ra, rb))}, nil
}

// ratDecimalString renders a rational whose denominator is a product of
// 2s and 5s (always true for sums of finite decimals) as a minimal
// decimal string, e.g. "12.5", "-0.03", "1E+30".
func ratDecimalString(r *big.Rat) string {
	if r.Sign() == 0 {
		return "0"
	}
	num := new(big.Int).Set(r.Num())   // signed
	den := new(big.Int).Set(r.Denom()) // positive
	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}
	// value = num/den. Scale num by 10^k until divisible by den.
	ten := big.NewInt(10)
	k := 0
	q := new(big.Int)
	rem := new(big.Int)
	for {
		q.QuoRem(num, den, rem)
		if rem.Sign() == 0 {
			break
		}
		num.Mul(num, ten)
		k++
		if k > 400 {
			// should be unreachable for decimal-derived rationals
			return "0"
		}
	}
	// value = q * 10^-k
	digits := q.String()
	var out string
	switch {
	case k == 0:
		out = digits
	case k < len(digits):
		out = digits[:len(digits)-k] + "." + digits[len(digits)-k:]
	default:
		out = "0." + strings.Repeat("0", k-len(digits)) + digits
	}
	out = strings.TrimRight(out, "0")
	out = strings.TrimSuffix(out, ".")
	if out == "" {
		out = "0"
	}
	if neg {
		out = "-" + out
	}
	return out
}
