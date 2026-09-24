package dynar

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Expression engine: tokenizer + recursive-descent parser for DynamoDB
// condition / filter / key-condition / projection / update expressions.
//
// Grammar (condition):
//
//	expr      := orExpr
//	orExpr    := andExpr ( OR andExpr )*
//	andExpr   := unary ( AND unary )*
//	unary     := NOT unary | primary
//	primary   := '(' orExpr ')' | predicate | operand cmpTail
//	cmpTail   := cmp operand | BETWEEN operand AND operand | IN '(' list ')'
//	predicate := attribute_exists '(' path ')' | attribute_not_exists '(' path ')'
//	            | attribute_type '(' operand ',' operand ')'
//	            | begins_with '(' operand ',' operand ')'
//	            | contains '(' operand ',' operand ')'
//	operand   := path | ':'name | size '(' path ')'
//	path      := identOrRef ( '.' identOrRef | '[' num ']' )*

type tokenKind int

const (
	tkEOF tokenKind = iota
	tkIdent
	tkNameRef // #name
	tkValRef  // :name
	tkNum
	tkLParen
	tkRParen
	tkLBracket
	tkRBracket
	tkComma
	tkDot
	tkPlus  // +
	tkMinus // -
	tkOp    // = <> < <= > >=
)

type token struct {
	kind tokenKind
	text string
}

func lexExpr(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{tkLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tkRParen, ")"})
			i++
		case c == '[':
			toks = append(toks, token{tkLBracket, "["})
			i++
		case c == ']':
			toks = append(toks, token{tkRBracket, "]"})
			i++
		case c == ',':
			toks = append(toks, token{tkComma, ","})
			i++
		case c == '.':
			toks = append(toks, token{tkDot, "."})
			i++
		case c == '#':
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			if j == i+1 {
				return nil, fmt.Errorf("invalid name reference at position %d", i)
			}
			toks = append(toks, token{tkNameRef, s[i:j]})
			i = j
		case c == ':':
			j := i + 1
			for j < len(s) && isValNameChar(s[j]) {
				j++
			}
			if j == i+1 {
				return nil, fmt.Errorf("invalid value reference at position %d", i)
			}
			toks = append(toks, token{tkValRef, s[i:j]})
			i = j
		case c == '=':
			toks = append(toks, token{tkOp, "="})
			i++
		case c == '+':
			toks = append(toks, token{tkPlus, "+"})
			i++
		case c == '-':
			toks = append(toks, token{tkMinus, "-"})
			i++
		case c == '<':
			if i+1 < len(s) && s[i+1] == '=' {
				toks = append(toks, token{tkOp, "<="})
				i += 2
			} else if i+1 < len(s) && s[i+1] == '>' {
				toks = append(toks, token{tkOp, "<>"})
				i += 2
			} else {
				toks = append(toks, token{tkOp, "<"})
				i++
			}
		case c == '>':
			if i+1 < len(s) && s[i+1] == '=' {
				toks = append(toks, token{tkOp, ">="})
				i += 2
			} else {
				toks = append(toks, token{tkOp, ">"})
				i++
			}
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			toks = append(toks, token{tkNum, s[i:j]})
			i = j
		case isNameChar(c):
			j := i
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			toks = append(toks, token{tkIdent, s[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", string(c), i)
		}
	}
	toks = append(toks, token{tkEOF, ""})
	return toks, nil
}

func isNameChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isValNameChar(c byte) bool {
	return isNameChar(c) || c == '.' || c == '-'
}

// pathElem is one segment of a document path.
type pathElem struct {
	name  string
	index int
	isIdx bool
}

type parser struct {
	toks []token
	pos  int
	env  *exprEnv
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) atEnd() bool { return p.toks[p.pos].kind == tkEOF }

func (p *parser) expect(k tokenKind, what string) error {
	if p.toks[p.pos].kind != k {
		return fmt.Errorf("expected %s, found %q", what, p.toks[p.pos].text)
	}
	p.pos++
	return nil
}

// peekKeyword reports whether the next token is the given keyword,
// matched case-insensitively.
func (p *parser) peekKeyword(kw string) bool {
	t := p.toks[p.pos]
	return t.kind == tkIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) takeKeyword(kw string) bool {
	if p.peekKeyword(kw) {
		p.pos++
		return true
	}
	return false
}

// ---- paths ----

// parsePath reads a document path: name | name.attr | name[0].attr ...
// Identifiers are validated against the reserved-word list.
func (p *parser) parsePath() ([]pathElem, error) {
	var path []pathElem
	t := p.next()
	switch t.kind {
	case tkIdent:
		if isReservedWord(t.text) {
			return nil, fmt.Errorf("attribute name %q is a reserved keyword; use ExpressionAttributeNames", t.text)
		}
		path = append(path, pathElem{name: t.text})
	case tkNameRef:
		n, err := p.env.resolveName(t.text)
		if err != nil {
			return nil, err
		}
		path = append(path, pathElem{name: n})
	default:
		return nil, fmt.Errorf("expected attribute name, found %q", t.text)
	}
	for {
		switch p.peek().kind {
		case tkDot:
			p.next()
			t := p.next()
			switch t.kind {
			case tkIdent:
				if isReservedWord(t.text) {
					return nil, fmt.Errorf("attribute name %q is a reserved keyword; use ExpressionAttributeNames", t.text)
				}
				path = append(path, pathElem{name: t.text})
			case tkNameRef:
				n, err := p.env.resolveName(t.text)
				if err != nil {
					return nil, err
				}
				path = append(path, pathElem{name: n})
			default:
				return nil, fmt.Errorf("expected attribute name after '.', found %q", t.text)
			}
		case tkLBracket:
			p.next()
			n := p.next()
			if n.kind != tkNum {
				return nil, fmt.Errorf("expected index in '[]', found %q", n.text)
			}
			var idx int
			fmt.Sscanf(n.text, "%d", &idx)
			path = append(path, pathElem{index: idx, isIdx: true})
			if err := p.expect(tkRBracket, "']'"); err != nil {
				return nil, err
			}
		default:
			return path, nil
		}
	}
}

// ---- operands ----

// operandKind distinguishes value references from document paths.
type operandKind int

const (
	opndPath  operandKind = iota
	opndValue             // :ref
	opndSize              // size(path)
)

type operand struct {
	kind operandKind
	path []pathElem
	vref string
}

func (p *parser) parseOperand() (operand, error) {
	t := p.peek()
	switch {
	case t.kind == tkValRef:
		p.next()
		return operand{kind: opndValue, vref: t.text}, nil
	case t.kind == tkIdent && strings.EqualFold(t.text, "size"):
		// size(path) as operand
		save := p.pos
		p.next()
		if p.peek().kind != tkLParen {
			p.pos = save
			path, err := p.parsePath()
			if err != nil {
				return operand{}, err
			}
			return operand{kind: opndPath, path: path}, nil
		}
		p.next() // '('
		path, err := p.parsePath()
		if err != nil {
			return operand{}, err
		}
		if err := p.expect(tkRParen, "')' after size(path)"); err != nil {
			return operand{}, err
		}
		return operand{kind: opndSize, path: path}, nil
	case t.kind == tkIdent || t.kind == tkNameRef:
		path, err := p.parsePath()
		if err != nil {
			return operand{}, err
		}
		return operand{kind: opndPath, path: path}, nil
	default:
		return operand{}, fmt.Errorf("expected attribute path or value reference, found %q", t.text)
	}
}

// resolveOperand evaluates an operand against an item.
// found=false means a document path did not resolve (value refs always resolve).
func resolveOperand(o operand, item map[string]types.AttributeValue, env *exprEnv) (types.AttributeValue, bool, error) {
	switch o.kind {
	case opndValue:
		v, err := env.resolveValue(o.vref)
		if err != nil {
			return nil, false, err
		}
		return v, true, nil
	case opndSize:
		v, ok := resolvePath(item, o.path)
		if !ok {
			return nil, false, nil
		}
		n, err := sizeOf(v)
		if err != nil {
			return nil, false, err
		}
		return &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", n)}, true, nil
	default:
		v, ok := resolvePath(item, o.path)
		return v, ok, nil
	}
}

// resolvePath walks a document path in an item.
func resolvePath(item map[string]types.AttributeValue, path []pathElem) (types.AttributeValue, bool) {
	if len(path) == 0 {
		return nil, false
	}
	cur, ok := item[path[0].name]
	if !ok {
		return nil, false
	}
	for _, e := range path[1:] {
		if e.isIdx {
			l, ok := cur.(*types.AttributeValueMemberL)
			if !ok || e.index >= len(l.Value) {
				return nil, false
			}
			cur = l.Value[e.index]
		} else {
			m, ok := cur.(*types.AttributeValueMemberM)
			if !ok {
				return nil, false
			}
			cur, ok = m.Value[e.name]
			if !ok {
				return nil, false
			}
		}
	}
	return cur, true
}

// ---- condition / filter / key-condition AST ----

type condNode interface{}

type orNode struct{ terms []condNode }
type andNode struct{ terms []condNode }
type notNode struct{ e condNode }
type cmpNode struct {
	op          string
	left, right operand
}
type betweenNode struct {
	v, lo, hi operand
}
type inNode struct {
	v    operand
	list []operand
}
type funcNode struct {
	name string // attribute_exists, attribute_not_exists, attribute_type, begins_with, contains
	args []operand
}

// parseCondition parses a full condition/filter expression.
func parseConditionExpr(s string, env *exprEnv) (condNode, error) {
	toks, err := lexExpr(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, env: env}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("unexpected token %q", p.peek().text)
	}
	return n, nil
}

func (p *parser) parseOr() (condNode, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if !p.peekKeyword("OR") {
		return first, nil
	}
	terms := []condNode{first}
	for p.takeKeyword("OR") {
		t, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		terms = append(terms, t)
	}
	return orNode{terms}, nil
}

func (p *parser) parseAnd() (condNode, error) {
	first, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	if !p.peekKeyword("AND") {
		return first, nil
	}
	terms := []condNode{first}
	for p.takeKeyword("AND") {
		t, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		terms = append(terms, t)
	}
	return andNode{terms}, nil
}

func (p *parser) parseUnary() (condNode, error) {
	if p.takeKeyword("NOT") {
		e, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return notNode{e}, nil
	}
	return p.parsePrimary()
}

var condFuncNames = map[string]int{
	"attribute_exists":     1,
	"attribute_not_exists": 1,
	"attribute_type":       2,
	"begins_with":          2,
	"contains":             2,
}

func (p *parser) parsePrimary() (condNode, error) {
	t := p.peek()
	if t.kind == tkLParen {
		p.next()
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tkRParen, "')'"); err != nil {
			return nil, err
		}
		return n, nil
	}
	// function call?
	if t.kind == tkIdent {
		if _, isFunc := condFuncNames[strings.ToLower(t.text)]; isFunc && p.toks[p.pos+1].kind == tkLParen {
			return p.parsePredicate()
		}
	}
	// comparison: operand followed by a comparator / BETWEEN / IN
	left, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	t = p.peek()
	switch {
	case t.kind == tkOp:
		p.next()
		right, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return cmpNode{op: t.text, left: left, right: right}, nil
	case p.peekKeyword("BETWEEN"):
		p.next()
		lo, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		if !p.takeKeyword("AND") {
			return nil, fmt.Errorf("expected AND in BETWEEN, found %q", p.peek().text)
		}
		hi, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return betweenNode{v: left, lo: lo, hi: hi}, nil
	case p.peekKeyword("IN"):
		p.next()
		if err := p.expect(tkLParen, "'(' after IN"); err != nil {
			return nil, err
		}
		var list []operand
		for {
			o, err := p.parseOperand()
			if err != nil {
				return nil, err
			}
			list = append(list, o)
			if p.peek().kind == tkComma {
				p.next()
				continue
			}
			break
		}
		if err := p.expect(tkRParen, "')' after IN list"); err != nil {
			return nil, err
		}
		return inNode{v: left, list: list}, nil
	default:
		return nil, fmt.Errorf("expected comparison operator, found %q", t.text)
	}
}

func (p *parser) parsePredicate() (condNode, error) {
	nameTok := p.next()
	name := strings.ToLower(nameTok.text)
	p.next() // '('
	arity := condFuncNames[name]
	var args []operand
	for i := 0; i < arity; i++ {
		o, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		args = append(args, o)
		if i+1 < arity {
			if err := p.expect(tkComma, "','"); err != nil {
				return nil, err
			}
		}
	}
	if err := p.expect(tkRParen, "')'"); err != nil {
		return nil, err
	}
	return funcNode{name: name, args: args}, nil
}

// ---- condition evaluation ----

func evalCond(n condNode, item map[string]types.AttributeValue, env *exprEnv) (bool, error) {
	switch c := n.(type) {
	case orNode:
		for _, t := range c.terms {
			ok, err := evalCond(t, item, env)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case andNode:
		for _, t := range c.terms {
			ok, err := evalCond(t, item, env)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	case notNode:
		ok, err := evalCond(c.e, item, env)
		if err != nil {
			return false, err
		}
		return !ok, nil
	case cmpNode:
		return evalCmp(c, item, env)
	case betweenNode:
		v, ok, err := resolveOperand(c.v, item, env)
		if err != nil {
			return false, err
		}
		lo, okL, err := resolveOperand(c.lo, item, env)
		if err != nil {
			return false, err
		}
		hi, okH, err := resolveOperand(c.hi, item, env)
		if err != nil {
			return false, err
		}
		if !ok || !okL || !okH {
			return false, nil
		}
		loCmp, err := compareOrder(v, lo)
		if err != nil {
			return false, nil
		}
		hiCmp, err := compareOrder(v, hi)
		if err != nil {
			return false, nil
		}
		return loCmp >= 0 && hiCmp <= 0, nil
	case inNode:
		v, ok, err := resolveOperand(c.v, item, env)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		for _, lo := range c.list {
			o, okO, err := resolveOperand(lo, item, env)
			if err != nil {
				return false, err
			}
			if !okO {
				continue
			}
			if avEqual(v, o) {
				return true, nil
			}
		}
		return false, nil
	case funcNode:
		return evalFunc(c, item, env)
	default:
		return false, fmt.Errorf("invalid expression node %T", n)
	}
}

func evalCmp(c cmpNode, item map[string]types.AttributeValue, env *exprEnv) (bool, error) {
	l, okL, err := resolveOperand(c.left, item, env)
	if err != nil {
		return false, err
	}
	r, okR, err := resolveOperand(c.right, item, env)
	if err != nil {
		return false, err
	}
	// A path that does not exist makes = false and <> true; ordering
	// comparisons are false (DynamoDB treats missing attrs as not matching).
	if !okL || !okR {
		switch c.op {
		case "<>":
			return true, nil
		default:
			return false, nil
		}
	}
	switch c.op {
	case "=":
		return avEqual(l, r), nil
	case "<>":
		return !avEqual(l, r), nil
	}
	cmp, err := compareOrder(l, r)
	if err != nil {
		// Cross-type or non-scalar ordering does not error; the condition
		// simply does not match (verified against DynamoDB Local).
		return false, nil
	}
	switch c.op {
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	}
	return false, fmt.Errorf("unknown operator %q", c.op)
}

func evalFunc(f funcNode, item map[string]types.AttributeValue, env *exprEnv) (bool, error) {
	switch f.name {
	case "attribute_exists", "attribute_not_exists":
		if f.args[0].kind != opndPath {
			return false, fmt.Errorf("%s requires an attribute path", f.name)
		}
		_, ok := resolvePath(item, f.args[0].path)
		if f.name == "attribute_exists" {
			return ok, nil
		}
		return !ok, nil
	case "attribute_type":
		v, ok, err := resolveOperand(f.args[0], item, env)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		typ, err := typeNameOperand(f.args[1], item, env)
		if err != nil {
			return false, err
		}
		return attrTypeName(v) == typ, nil
	case "begins_with":
		v, ok, err := resolveOperand(f.args[0], item, env)
		if err != nil {
			return false, err
		}
		arg, okA, err := resolveOperand(f.args[1], item, env)
		if err != nil {
			return false, err
		}
		if !ok || !okA {
			return false, nil
		}
		return beginsWith(v, arg)
	case "contains":
		v, ok, err := resolveOperand(f.args[0], item, env)
		if err != nil {
			return false, err
		}
		arg, okA, err := resolveOperand(f.args[1], item, env)
		if err != nil {
			return false, err
		}
		if !ok || !okA {
			return false, nil
		}
		return contains(v, arg)
	default:
		return false, fmt.Errorf("unsupported function %s", f.name)
	}
}

// typeNameOperand resolves the type argument of attribute_type:
// either a :ref holding an S like "N", or a bare type word.
func typeNameOperand(o operand, item map[string]types.AttributeValue, env *exprEnv) (string, error) {
	if o.kind == opndValue {
		v, err := env.resolveValue(o.vref)
		if err != nil {
			return "", err
		}
		s, ok := v.(*types.AttributeValueMemberS)
		if !ok {
			return "", fmt.Errorf("attribute_type requires a string type argument")
		}
		return strings.ToUpper(s.Value), nil
	}
	if o.kind == opndPath && len(o.path) == 1 && !o.path[0].isIdx {
		return strings.ToUpper(o.path[0].name), nil
	}
	return "", fmt.Errorf("invalid attribute_type argument")
}

func beginsWith(v, prefix types.AttributeValue) (bool, error) {
	switch a := v.(type) {
	case *types.AttributeValueMemberS:
		p, ok := prefix.(*types.AttributeValueMemberS)
		if !ok {
			return false, nil
		}
		return strings.HasPrefix(a.Value, p.Value), nil
	case *types.AttributeValueMemberB:
		p, ok := prefix.(*types.AttributeValueMemberB)
		if !ok {
			return false, nil
		}
		return bytes.HasPrefix(a.Value, p.Value), nil
	default:
		return false, fmt.Errorf("begins_with requires S or B operand")
	}
}

func contains(v, arg types.AttributeValue) (bool, error) {
	switch a := v.(type) {
	case *types.AttributeValueMemberS:
		s, ok := arg.(*types.AttributeValueMemberS)
		if !ok {
			return false, nil
		}
		return strings.Contains(a.Value, s.Value), nil
	case *types.AttributeValueMemberB:
		b, ok := arg.(*types.AttributeValueMemberB)
		if !ok {
			return false, nil
		}
		return bytes.Contains(a.Value, b.Value), nil
	case *types.AttributeValueMemberSS:
		s, ok := arg.(*types.AttributeValueMemberS)
		if !ok {
			return false, nil
		}
		for _, e := range a.Value {
			if e == s.Value {
				return true, nil
			}
		}
		return false, nil
	case *types.AttributeValueMemberNS:
		n, ok := arg.(*types.AttributeValueMemberN)
		if !ok {
			return false, nil
		}
		for _, e := range a.Value {
			if numbersEqual(e, n.Value) {
				return true, nil
			}
		}
		return false, nil
	case *types.AttributeValueMemberBS:
		b, ok := arg.(*types.AttributeValueMemberB)
		if !ok {
			return false, nil
		}
		for _, e := range a.Value {
			if bytes.Equal(e, b.Value) {
				return true, nil
			}
		}
		return false, nil
	case *types.AttributeValueMemberL:
		for _, e := range a.Value {
			if avEqual(e, arg) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("contains is not supported for %s", attrTypeName(v))
	}
}

func sizeOf(v types.AttributeValue) (int64, error) {
	switch a := v.(type) {
	case *types.AttributeValueMemberS:
		return int64(len(a.Value)), nil
	case *types.AttributeValueMemberB:
		return int64(len(a.Value)), nil
	case *types.AttributeValueMemberSS:
		return int64(len(a.Value)), nil
	case *types.AttributeValueMemberNS:
		return int64(len(a.Value)), nil
	case *types.AttributeValueMemberBS:
		return int64(len(a.Value)), nil
	case *types.AttributeValueMemberL:
		return int64(len(a.Value)), nil
	case *types.AttributeValueMemberM:
		return int64(len(a.Value)), nil
	default:
		return 0, fmt.Errorf("size() is not supported for %s", attrTypeName(v))
	}
}

func attrTypeName(v types.AttributeValue) string {
	switch v.(type) {
	case *types.AttributeValueMemberS:
		return "S"
	case *types.AttributeValueMemberN:
		return "N"
	case *types.AttributeValueMemberB:
		return "B"
	case *types.AttributeValueMemberBOOL:
		return "BOOL"
	case *types.AttributeValueMemberNULL:
		return "NULL"
	case *types.AttributeValueMemberSS:
		return "SS"
	case *types.AttributeValueMemberNS:
		return "NS"
	case *types.AttributeValueMemberBS:
		return "BS"
	case *types.AttributeValueMemberL:
		return "L"
	case *types.AttributeValueMemberM:
		return "M"
	default:
		return "?"
	}
}

// avEqual implements DynamoDB equality: same type and same value, with
// numbers compared numerically and sets compared as sets.
func avEqual(a, b types.AttributeValue) bool {
	switch av := a.(type) {
	case *types.AttributeValueMemberS:
		bv, ok := b.(*types.AttributeValueMemberS)
		return ok && av.Value == bv.Value
	case *types.AttributeValueMemberN:
		bv, ok := b.(*types.AttributeValueMemberN)
		return ok && numbersEqual(av.Value, bv.Value)
	case *types.AttributeValueMemberB:
		bv, ok := b.(*types.AttributeValueMemberB)
		return ok && bytes.Equal(av.Value, bv.Value)
	case *types.AttributeValueMemberBOOL:
		bv, ok := b.(*types.AttributeValueMemberBOOL)
		return ok && av.Value == bv.Value
	case *types.AttributeValueMemberNULL:
		bv, ok := b.(*types.AttributeValueMemberNULL)
		return ok && av.Value == bv.Value
	case *types.AttributeValueMemberSS:
		bv, ok := b.(*types.AttributeValueMemberSS)
		return ok && stringSetEqual(av.Value, bv.Value)
	case *types.AttributeValueMemberNS:
		bv, ok := b.(*types.AttributeValueMemberNS)
		if !ok || len(av.Value) != len(bv.Value) {
			return false
		}
		used := make([]bool, len(bv.Value))
		for _, e := range av.Value {
			found := false
			for i, f := range bv.Value {
				if !used[i] && numbersEqual(e, f) {
					used[i] = true
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	case *types.AttributeValueMemberBS:
		bv, ok := b.(*types.AttributeValueMemberBS)
		if !ok || len(av.Value) != len(bv.Value) {
			return false
		}
		used := make([]bool, len(bv.Value))
		for _, e := range av.Value {
			found := false
			for i, f := range bv.Value {
				if !used[i] && bytes.Equal(e, f) {
					used[i] = true
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	case *types.AttributeValueMemberL:
		bv, ok := b.(*types.AttributeValueMemberL)
		if !ok || len(av.Value) != len(bv.Value) {
			return false
		}
		for i := range av.Value {
			if !avEqual(av.Value[i], bv.Value[i]) {
				return false
			}
		}
		return true
	case *types.AttributeValueMemberM:
		bv, ok := b.(*types.AttributeValueMemberM)
		if !ok || len(av.Value) != len(bv.Value) {
			return false
		}
		for k, e := range av.Value {
			f, ok := bv.Value[k]
			if !ok || !avEqual(e, f) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
		if m[s] < 0 {
			return false
		}
	}
	return true
}

// compareOrder orders same-type scalars. Returns an error for
// non-orderable or mismatched types, matching DynamoDB's
// "Invalid condition" behavior.
func compareOrder(a, b types.AttributeValue) (int, error) {
	switch av := a.(type) {
	case *types.AttributeValueMemberS:
		bv, ok := b.(*types.AttributeValueMemberS)
		if !ok {
			return 0, fmt.Errorf("comparison of S and %s is not allowed", attrTypeName(b))
		}
		return strings.Compare(av.Value, bv.Value), nil
	case *types.AttributeValueMemberN:
		bv, ok := b.(*types.AttributeValueMemberN)
		if !ok {
			return 0, fmt.Errorf("comparison of N and %s is not allowed", attrTypeName(b))
		}
		return compareNumbers(av.Value, bv.Value)
	case *types.AttributeValueMemberB:
		bv, ok := b.(*types.AttributeValueMemberB)
		if !ok {
			return 0, fmt.Errorf("comparison of B and %s is not allowed", attrTypeName(b))
		}
		return bytes.Compare(av.Value, bv.Value), nil
	default:
		return 0, fmt.Errorf("comparison of %s is not allowed", attrTypeName(a))
	}
}
