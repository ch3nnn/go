// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"fmt"
	"go/constant"
	"go/token"
	"internal/types/errors"
	"math"
	"math/big"
	"unicode"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

func roundFloat(v constant.Value, sz int64) constant.Value {
	switch sz {
	case 4:
		f, _ := constant.Float32Val(v)
		return makeFloat64(float64(f))
	case 8:
		f, _ := constant.Float64Val(v)
		return makeFloat64(f)
	}
	base.Fatalf("unexpected size: %v", sz)
	panic("unreachable")
}

// truncate float literal fv to 32-bit or 64-bit precision
// according to type; return truncated value.
func truncfltlit(v constant.Value, t *types.Type) constant.Value {
	if t.IsUntyped() {
		return v
	}

	return roundFloat(v, t.Size())
}

// truncate Real and Imag parts of Mpcplx to 32-bit or 64-bit
// precision, according to type; return truncated value. In case of
// overflow, calls Errorf but does not truncate the input value.
func trunccmplxlit(v constant.Value, t *types.Type) constant.Value {
	if t.IsUntyped() {
		return v
	}

	fsz := t.Size() / 2
	return makeComplex(roundFloat(constant.Real(v), fsz), roundFloat(constant.Imag(v), fsz))
}

// TODO(mdempsky): Replace these with better APIs.
func convlit(n ir.Node, t *types.Type) ir.Node    { return convlit1(n, t, false, nil) }
func DefaultLit(n ir.Node, t *types.Type) ir.Node { return convlit1(n, t, false, nil) }

// convlit1 converts an untyped expression n to type t. If n already
// has a type, convlit1 has no effect.
//
// For explicit conversions, t must be non-nil, and integer-to-string
// conversions are allowed.
//
// For implicit conversions (e.g., assignments), t may be nil; if so,
// n is converted to its default type.
//
// If there's an error converting n to t, context is used in the error
// message.
func convlit1(n ir.Node, t *types.Type, explicit bool, context func() string) ir.Node {
	if explicit && t == nil {
		base.Fatalf("explicit conversion missing type")
	}
	if t != nil && t.IsUntyped() {
		base.Fatalf("bad conversion to untyped: %v", t)
	}

	if n == nil || n.Type() == nil {
		// Allow sloppy callers.
		return n
	}
	if !n.Type().IsUntyped() {
		// Already typed; nothing to do.
		return n
	}

	// Nil is technically not a constant, so handle it specially.
	if n.Type().Kind() == types.TNIL {
		if n.Op() != ir.ONIL {
			base.Fatalf("unexpected op: %v (%v)", n, n.Op())
		}
		n = ir.Copy(n)
		if t == nil {
			base.Fatalf("use of untyped nil")
		}

		if !t.HasNil() {
			// Leave for caller to handle.
			return n
		}

		n.SetType(t)
		return n
	}

	if t == nil || !ir.OKForConst[t.Kind()] {
		t = defaultType(n.Type())
	}

	switch n.Op() {
	default:
		base.Fatalf("unexpected untyped expression: %v", n)

	case ir.OLITERAL:
		v := convertVal(n.Val(), t, explicit)
		if v.Kind() == constant.Unknown {
			n = ir.NewConstExpr(n.Val(), n)
			break
		}
		n = ir.NewConstExpr(v, n)
		n.SetType(t)
		return n

	case ir.OPLUS, ir.ONEG, ir.OBITNOT, ir.ONOT, ir.OREAL, ir.OIMAG:
		ot := operandType(n.Op(), t)
		if ot == nil {
			n = DefaultLit(n, nil)
			break
		}

		n := n.(*ir.UnaryExpr)
		n.X = convlit(n.X, ot)
		if n.X.Type() == nil {
			n.SetType(nil)
			return n
		}
		n.SetType(t)
		return n

	case ir.OADD, ir.OSUB, ir.OMUL, ir.ODIV, ir.OMOD, ir.OOR, ir.OXOR, ir.OAND, ir.OANDNOT, ir.OOROR, ir.OANDAND, ir.OCOMPLEX:
		ot := operandType(n.Op(), t)
		if ot == nil {
			n = DefaultLit(n, nil)
			break
		}

		var l, r ir.Node
		switch n := n.(type) {
		case *ir.BinaryExpr:
			n.X = convlit(n.X, ot)
			n.Y = convlit(n.Y, ot)
			l, r = n.X, n.Y
		case *ir.LogicalExpr:
			n.X = convlit(n.X, ot)
			n.Y = convlit(n.Y, ot)
			l, r = n.X, n.Y
		}

		if l.Type() == nil || r.Type() == nil {
			n.SetType(nil)
			return n
		}
		if !types.Identical(l.Type(), r.Type()) {
			base.Errorf("invalid operation: %v (mismatched types %v and %v)", n, l.Type(), r.Type())
			n.SetType(nil)
			return n
		}

		n.SetType(t)
		return n

	case ir.OEQ, ir.ONE, ir.OLT, ir.OLE, ir.OGT, ir.OGE:
		n := n.(*ir.BinaryExpr)
		if !t.IsBoolean() {
			break
		}
		n.SetType(t)
		return n

	case ir.OLSH, ir.ORSH:
		n := n.(*ir.BinaryExpr)
		n.X = convlit1(n.X, t, explicit, nil)
		n.SetType(n.X.Type())
		if n.Type() != nil && !n.Type().IsInteger() {
			base.Errorf("invalid operation: %v (shift of type %v)", n, n.Type())
			n.SetType(nil)
		}
		return n
	}

	if explicit {
		base.Fatalf("cannot convert %L to type %v", n, t)
	} else if context != nil {
		base.Fatalf("cannot use %L as type %v in %s", n, t, context())
	} else {
		base.Fatalf("cannot use %L as type %v", n, t)
	}

	n.SetType(nil)
	return n
}

func operandType(op ir.Op, t *types.Type) *types.Type {
	switch op {
	case ir.OCOMPLEX:
		if t.IsComplex() {
			return types.FloatForComplex(t)
		}
	case ir.OREAL, ir.OIMAG:
		if t.IsFloat() {
			return types.ComplexForFloat(t)
		}
	default:
		if okfor[op][t.Kind()] {
			return t
		}
	}
	return nil
}

// convertVal converts v into a representation appropriate for t. If
// no such representation exists, it returns Val{} instead.
//
// If explicit is true, then conversions from integer to string are
// also allowed.
func convertVal(v constant.Value, t *types.Type, explicit bool) constant.Value {
	switch ct := v.Kind(); ct {
	case constant.Bool:
		if t.IsBoolean() {
			return v
		}

	case constant.String:
		if t.IsString() {
			return v
		}

	case constant.Int:
		if explicit && t.IsString() {
			return tostr(v)
		}
		fallthrough
	case constant.Float, constant.Complex:
		switch {
		case t.IsInteger():
			v = toint(v)
			return v
		case t.IsFloat():
			v = toflt(v)
			v = truncfltlit(v, t)
			return v
		case t.IsComplex():
			v = tocplx(v)
			v = trunccmplxlit(v, t)
			return v
		}
	}

	return constant.MakeUnknown()
}

func tocplx(v constant.Value) constant.Value {
	return constant.ToComplex(v)
}

func toflt(v constant.Value) constant.Value {
	if v.Kind() == constant.Complex {
		v = constant.Real(v)
	}

	return constant.ToFloat(v)
}

func toint(v constant.Value) constant.Value {
	if v.Kind() == constant.Complex {
		v = constant.Real(v)
	}

	if v := constant.ToInt(v); v.Kind() == constant.Int {
		return v
	}

	// The value of v cannot be represented as an integer;
	// so we need to print an error message.
	// Unfortunately some float values cannot be
	// reasonably formatted for inclusion in an error
	// message (example: 1 + 1e-100), so first we try to
	// format the float; if the truncation resulted in
	// something that looks like an integer we omit the
	// value from the error message.
	// (See issue #11371).
	f := ir.BigFloat(v)
	if f.MantExp(nil) > 2*ir.ConstPrec {
		base.Errorf("integer too large")
	} else {
		var t big.Float
		t.Parse(fmt.Sprint(v), 0)
		if t.IsInt() {
			base.Errorf("constant truncated to integer")
		} else {
			base.Errorf("constant %v truncated to integer", v)
		}
	}

	// Prevent follow-on errors.
	// TODO(mdempsky): Use constant.MakeUnknown() instead.
	return constant.MakeInt64(1)
}

func tostr(v constant.Value) constant.Value {
	if v.Kind() == constant.Int {
		r := unicode.ReplacementChar
		if x, ok := constant.Uint64Val(v); ok && x <= unicode.MaxRune {
			r = rune(x)
		}
		v = constant.MakeString(string(r))
	}
	return v
}

func makeFloat64(f float64) constant.Value {
	if math.IsInf(f, 0) {
		base.Fatalf("infinity is not a valid constant")
	}
	return constant.MakeFloat64(f)
}

func makeComplex(real, imag constant.Value) constant.Value {
	return constant.BinaryOp(constant.ToFloat(real), token.ADD, constant.MakeImag(constant.ToFloat(imag)))
}

// For matching historical "constant OP overflow" error messages.
// TODO(mdempsky): Replace with error messages like go/types uses.
var overflowNames = [...]string{
	ir.OADD:    "addition",
	ir.OSUB:    "subtraction",
	ir.OMUL:    "multiplication",
	ir.OLSH:    "shift",
	ir.OXOR:    "bitwise XOR",
	ir.OBITNOT: "bitwise complement",
}

// OrigConst returns an OLITERAL with orig n and value v.
func OrigConst(n ir.Node, v constant.Value) ir.Node {
	lno := ir.SetPos(n)
	v = convertVal(v, n.Type(), false)
	base.Pos = lno

	switch v.Kind() {
	case constant.Int:
		if constant.BitLen(v) <= ir.ConstPrec {
			break
		}
		fallthrough
	case constant.Unknown:
		what := overflowNames[n.Op()]
		if what == "" {
			base.Fatalf("unexpected overflow: %v", n.Op())
		}
		base.ErrorfAt(n.Pos(), errors.NumericOverflow, "constant %v overflow", what)
		n.SetType(nil)
		return n
	}

	return ir.NewConstExpr(v, n)
}

func OrigBool(n ir.Node, v bool) ir.Node {
	return OrigConst(n, constant.MakeBool(v))
}

func OrigInt(n ir.Node, v int64) ir.Node {
	return OrigConst(n, constant.MakeInt64(v))
}

// DefaultLit on both nodes simultaneously;
// if they're both ideal going in they better
// get the same type going out.
// force means must assign concrete (non-ideal) type.
// The results of defaultlit2 MUST be assigned back to l and r, e.g.
//
//	n.Left, n.Right = defaultlit2(n.Left, n.Right, force)
func defaultlit2(l ir.Node, r ir.Node, force bool) (ir.Node, ir.Node) {
	if l.Type() == nil || r.Type() == nil {
		return l, r
	}

	if !l.Type().IsInterface() && !r.Type().IsInterface() {
		// Can't mix bool with non-bool, string with non-string.
		if l.Type().IsBoolean() != r.Type().IsBoolean() {
			return l, r
		}
		if l.Type().IsString() != r.Type().IsString() {
			return l, r
		}
	}

	if !l.Type().IsUntyped() {
		r = convlit(r, l.Type())
		return l, r
	}

	if !r.Type().IsUntyped() {
		l = convlit(l, r.Type())
		return l, r
	}

	if !force {
		return l, r
	}

	// Can't mix nil with anything untyped.
	if ir.IsNil(l) || ir.IsNil(r) {
		return l, r
	}
	t := defaultType(mixUntyped(l.Type(), r.Type()))
	l = convlit(l, t)
	r = convlit(r, t)
	return l, r
}

func mixUntyped(t1, t2 *types.Type) *types.Type {
	if t1 == t2 {
		return t1
	}

	rank := func(t *types.Type) int {
		switch t {
		case types.UntypedInt:
			return 0
		case types.UntypedRune:
			return 1
		case types.UntypedFloat:
			return 2
		case types.UntypedComplex:
			return 3
		}
		base.Fatalf("bad type %v", t)
		panic("unreachable")
	}

	if rank(t2) > rank(t1) {
		return t2
	}
	return t1
}

func defaultType(t *types.Type) *types.Type {
	if !t.IsUntyped() || t.Kind() == types.TNIL {
		return t
	}

	switch t {
	case types.UntypedBool:
		return types.Types[types.TBOOL]
	case types.UntypedString:
		return types.Types[types.TSTRING]
	case types.UntypedInt:
		return types.Types[types.TINT]
	case types.UntypedRune:
		return types.RuneType
	case types.UntypedFloat:
		return types.Types[types.TFLOAT64]
	case types.UntypedComplex:
		return types.Types[types.TCOMPLEX128]
	}

	base.Fatalf("bad type %v", t)
	return nil
}

// IndexConst checks if Node n contains a constant expression
// representable as a non-negative int and returns its value.
// If n is not a constant expression, not representable as an
// integer, or negative, it returns -1. If n is too large, it
// returns -2.
func IndexConst(n ir.Node) int64 {
	if n.Op() != ir.OLITERAL {
		return -1
	}
	if !n.Type().IsInteger() && n.Type().Kind() != types.TIDEAL {
		return -1
	}

	v := toint(n.Val())
	if v.Kind() != constant.Int || constant.Sign(v) < 0 {
		return -1
	}
	if ir.ConstOverflow(v, types.Types[types.TINT]) {
		return -2
	}
	return ir.IntVal(types.Types[types.TINT], v)
}

// callOrChan reports whether n is a call or channel operation.
func callOrChan(n ir.Node) bool {
	switch n.Op() {
	case ir.OAPPEND,
		ir.OCALL,
		ir.OCALLFUNC,
		ir.OCALLINTER,
		ir.OCALLMETH,
		ir.OCAP,
		ir.OCLEAR,
		ir.OCLOSE,
		ir.OCOMPLEX,
		ir.OCOPY,
		ir.ODELETE,
		ir.OIMAG,
		ir.OLEN,
		ir.OMAKE,
		ir.OMAX,
		ir.OMIN,
		ir.ONEW,
		ir.OPANIC,
		ir.OPRINT,
		ir.OPRINTN,
		ir.OREAL,
		ir.ORECOVER,
		ir.ORECOVERFP,
		ir.ORECV,
		ir.OUNSAFEADD,
		ir.OUNSAFESLICE,
		ir.OUNSAFESLICEDATA,
		ir.OUNSAFESTRING,
		ir.OUNSAFESTRINGDATA:
		return true
	}
	return false
}

// evalunsafe evaluates a package unsafe operation and returns the result.
func evalunsafe(n ir.Node) int64 {
	switch n.Op() {
	case ir.OALIGNOF, ir.OSIZEOF:
		n := n.(*ir.UnaryExpr)
		n.X = Expr(n.X)
		n.X = DefaultLit(n.X, nil)
		tr := n.X.Type()
		if tr == nil {
			return 0
		}
		types.CalcSize(tr)
		if n.Op() == ir.OALIGNOF {
			return tr.Alignment()
		}
		return tr.Size()

	case ir.OOFFSETOF:
		// must be a selector.
		n := n.(*ir.UnaryExpr)
		// ODOT and ODOTPTR are allowed in case the OXDOT transformation has
		// already happened (e.g. during -G=3 stenciling).
		if n.X.Op() != ir.OXDOT && n.X.Op() != ir.ODOT && n.X.Op() != ir.ODOTPTR {
			base.Errorf("invalid expression %v", n)
			return 0
		}
		sel := n.X.(*ir.SelectorExpr)

		// Remember base of selector to find it back after dot insertion.
		// Since r->left may be mutated by typechecking, check it explicitly
		// first to track it correctly.
		sel.X = Expr(sel.X)
		sbase := sel.X

		// Implicit dot may already be resolved for instantiating generic function. So we
		// need to remove any implicit dot until we reach the first non-implicit one, it's
		// the right base selector. See issue #53137.
		var clobberBase func(n ir.Node) ir.Node
		clobberBase = func(n ir.Node) ir.Node {
			if sel, ok := n.(*ir.SelectorExpr); ok && sel.Implicit() {
				return clobberBase(sel.X)
			}
			return n
		}
		sbase = clobberBase(sbase)

		tsel := Expr(sel)
		n.X = tsel
		if tsel.Type() == nil {
			return 0
		}
		switch tsel.Op() {
		case ir.ODOT, ir.ODOTPTR:
			break
		case ir.OMETHVALUE:
			base.Errorf("invalid expression %v: argument is a method value", n)
			return 0
		default:
			base.Errorf("invalid expression %v", n)
			return 0
		}

		// Sum offsets for dots until we reach sbase.
		var v int64
		var next ir.Node
		for r := tsel; r != sbase; r = next {
			switch r.Op() {
			case ir.ODOTPTR:
				// For Offsetof(s.f), s may itself be a pointer,
				// but accessing f must not otherwise involve
				// indirection via embedded pointer types.
				r := r.(*ir.SelectorExpr)
				if r.X != sbase {
					base.Errorf("invalid expression %v: selector implies indirection of embedded %v", n, r.X)
					return 0
				}
				fallthrough
			case ir.ODOT:
				r := r.(*ir.SelectorExpr)
				v += r.Offset()
				next = r.X
			default:
				ir.Dump("unsafenmagic", tsel)
				base.Fatalf("impossible %v node after dot insertion", r.Op())
			}
		}
		return v
	}

	base.Fatalf("unexpected op %v", n.Op())
	return 0
}
