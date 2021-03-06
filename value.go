// Copyright 2011 The llgo Authors.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package llgo

import (
	"code.google.com/p/go.tools/go/exact"
	"code.google.com/p/go.tools/go/types"
	"fmt"
	"github.com/axw/gollvm/llvm"
	"go/token"
)

// Value is an interface for representing values returned by Go expressions.
type Value interface {
	// BinaryOp applies the specified binary operator to this value and the
	// specified right-hand operand, and returns a new Value.
	BinaryOp(op token.Token, rhs Value) Value

	// UnaryOp applies the specified unary operator and returns a new Value.
	UnaryOp(op token.Token) Value

	// Convert returns a new Value which has been converted to the specified
	// type.
	Convert(typ types.Type) Value

	// LLVMValue returns an llvm.Value for this value.
	LLVMValue() llvm.Value

	// Type returns the Type of the value.
	Type() types.Type
}

// LLVMValue represents a dynamic value produced as the result of an
// expression.
type LLVMValue struct {
	compiler *compiler
	value    llvm.Value
	typ      types.Type
}

func (v LLVMValue) String() string {
	return fmt.Sprintf("[llgo.LLVMValue typ:%s value:%v]", v.typ, v.value)
}

// Create a new dynamic value from a (LLVM Value, Type) pair.
func (c *compiler) NewValue(v llvm.Value, t types.Type) *LLVMValue {
	return &LLVMValue{c, v, t}
}

func (c *compiler) NewConstValue(v exact.Value, typ types.Type) *LLVMValue {
	switch {
	case v == nil:
		llvmtyp := c.types.ToLLVM(typ)
		return c.NewValue(llvm.ConstNull(llvmtyp), typ)

	case isString(typ):
		if isUntyped(typ) {
			typ = types.Typ[types.String]
		}
		llvmtyp := c.types.ToLLVM(typ)
		strval := exact.StringVal(v)
		strlen := len(strval)
		i8ptr := llvm.PointerType(llvm.Int8Type(), 0)
		var ptr llvm.Value
		if strlen > 0 {
			init := llvm.ConstString(strval, false)
			ptr = llvm.AddGlobal(c.module.Module, init.Type(), "")
			ptr.SetInitializer(init)
			ptr = llvm.ConstBitCast(ptr, i8ptr)
		} else {
			ptr = llvm.ConstNull(i8ptr)
		}
		len_ := llvm.ConstInt(c.types.inttype, uint64(strlen), false)
		llvmvalue := llvm.Undef(llvmtyp)
		llvmvalue = llvm.ConstInsertValue(llvmvalue, ptr, []uint32{0})
		llvmvalue = llvm.ConstInsertValue(llvmvalue, len_, []uint32{1})
		return c.NewValue(llvmvalue, typ)

	case isInteger(typ):
		if isUntyped(typ) {
			typ = types.Typ[types.Int]
		}
		llvmtyp := c.types.ToLLVM(typ)
		var llvmvalue llvm.Value
		if isUnsigned(typ) {
			v, _ := exact.Uint64Val(v)
			llvmvalue = llvm.ConstInt(llvmtyp, v, false)
		} else {
			v, _ := exact.Int64Val(v)
			llvmvalue = llvm.ConstInt(llvmtyp, uint64(v), true)
		}
		return c.NewValue(llvmvalue, typ)

	case isBoolean(typ):
		if isUntyped(typ) {
			typ = types.Typ[types.Bool]
		}
		var llvmvalue llvm.Value
		if exact.BoolVal(v) {
			llvmvalue = llvm.ConstAllOnes(llvm.Int1Type())
		} else {
			llvmvalue = llvm.ConstNull(llvm.Int1Type())
		}
		return c.NewValue(llvmvalue, typ)

	case isFloat(typ):
		if isUntyped(typ) {
			typ = types.Typ[types.Float64]
		}
		llvmtyp := c.types.ToLLVM(typ)
		floatval, _ := exact.Float64Val(v)
		llvmvalue := llvm.ConstFloat(llvmtyp, floatval)
		return c.NewValue(llvmvalue, typ)

	case typ == types.Typ[types.UnsafePointer]:
		llvmtyp := c.types.ToLLVM(typ)
		v, _ := exact.Uint64Val(v)
		llvmvalue := llvm.ConstInt(llvmtyp, v, false)
		return c.NewValue(llvmvalue, typ)

	case isComplex(typ):
		if isUntyped(typ) {
			typ = types.Typ[types.Complex128]
		}
		llvmtyp := c.types.ToLLVM(typ)
		floattyp := llvmtyp.StructElementTypes()[0]
		llvmvalue := llvm.ConstNull(llvmtyp)
		realv := exact.Real(v)
		imagv := exact.Imag(v)
		realfloatval, _ := exact.Float64Val(realv)
		imagfloatval, _ := exact.Float64Val(imagv)
		llvmre := llvm.ConstFloat(floattyp, realfloatval)
		llvmim := llvm.ConstFloat(floattyp, imagfloatval)
		llvmvalue = llvm.ConstInsertValue(llvmvalue, llvmre, []uint32{0})
		llvmvalue = llvm.ConstInsertValue(llvmvalue, llvmim, []uint32{1})
		return c.NewValue(llvmvalue, typ)
	}

	// Special case for string -> [](byte|rune)
	if u, ok := typ.Underlying().(*types.Slice); ok && isInteger(u.Elem()) {
		if v.Kind() == exact.String {
			strval := c.NewConstValue(v, types.Typ[types.String])
			return strval.Convert(typ).(*LLVMValue)
		}
	}

	panic(fmt.Sprintf("unhandled: t=%s(%T), v=%v(%T)", typ, typ, v, v))
}

///////////////////////////////////////////////////////////////////////////////
// LLVMValue methods

func (lhs *LLVMValue) BinaryOp(op token.Token, rhs_ Value) Value {
	if op == token.NEQ {
		result := lhs.BinaryOp(token.EQL, rhs_)
		return result.UnaryOp(token.NOT)
	}

	var result llvm.Value
	c := lhs.compiler
	b := lhs.compiler.builder

	rhs := rhs_.(*LLVMValue)
	switch typ := lhs.typ.Underlying().(type) {
	case *types.Struct:
		// TODO(axw) use runtime equality algorithm (will be suitably inlined).
		// For now, we use compare all fields unconditionally and bitwise AND
		// to avoid branching (i.e. so we don't create additional blocks).
		var value Value = c.NewValue(boolLLVMValue(true), types.Typ[types.Bool])
		for i := 0; i < typ.NumFields(); i++ {
			t := typ.Field(i).Type()
			lhs := c.NewValue(b.CreateExtractValue(lhs.LLVMValue(), i, ""), t)
			rhs := c.NewValue(b.CreateExtractValue(rhs.LLVMValue(), i, ""), t)
			value = value.BinaryOp(token.AND, lhs.BinaryOp(token.EQL, rhs))
		}
		return value

	case *types.Slice:
		// []T == nil
		isnil := b.CreateIsNull(b.CreateExtractValue(lhs.LLVMValue(), 0, ""), "")
		return c.NewValue(isnil, types.Typ[types.Bool])

	case *types.Signature:
		// func == nil
		isnil := b.CreateIsNull(b.CreateExtractValue(lhs.LLVMValue(), 0, ""), "")
		return c.NewValue(isnil, types.Typ[types.Bool])

	case *types.Interface:
		return c.compareInterfaces(lhs, rhs)
	}

	// Strings.
	if isString(lhs.typ) {
		if isString(rhs.typ) {
			switch op {
			case token.ADD:
				return c.concatenateStrings(lhs, rhs)
			case token.EQL, token.LSS, token.GTR, token.LEQ, token.GEQ:
				return c.compareStrings(lhs, rhs, op)
			default:
				panic(fmt.Sprint("Unimplemented operator: ", op))
			}
		}
		panic("unimplemented")
	}

	// Complex numbers.
	if isComplex(lhs.typ) {
		// XXX Should we represent complex numbers as vectors?
		lhsval := lhs.LLVMValue()
		rhsval := rhs.LLVMValue()
		a_ := b.CreateExtractValue(lhsval, 0, "")
		b_ := b.CreateExtractValue(lhsval, 1, "")
		c_ := b.CreateExtractValue(rhsval, 0, "")
		d_ := b.CreateExtractValue(rhsval, 1, "")
		switch op {
		case token.QUO:
			// (a+bi)/(c+di) = (ac+bd)/(c**2+d**2) + (bc-ad)/(c**2+d**2)i
			ac := b.CreateFMul(a_, c_, "")
			bd := b.CreateFMul(b_, d_, "")
			bc := b.CreateFMul(b_, c_, "")
			ad := b.CreateFMul(a_, d_, "")
			cpow2 := b.CreateFMul(c_, c_, "")
			dpow2 := b.CreateFMul(d_, d_, "")
			denom := b.CreateFAdd(cpow2, dpow2, "")
			realnumer := b.CreateFAdd(ac, bd, "")
			imagnumer := b.CreateFSub(bc, ad, "")
			real_ := b.CreateFDiv(realnumer, denom, "")
			imag_ := b.CreateFDiv(imagnumer, denom, "")
			lhsval = b.CreateInsertValue(lhsval, real_, 0, "")
			result = b.CreateInsertValue(lhsval, imag_, 1, "")
		case token.MUL:
			// (a+bi)(c+di) = (ac-bd)+(bc+ad)i
			ac := b.CreateFMul(a_, c_, "")
			bd := b.CreateFMul(b_, d_, "")
			bc := b.CreateFMul(b_, c_, "")
			ad := b.CreateFMul(a_, d_, "")
			real_ := b.CreateFSub(ac, bd, "")
			imag_ := b.CreateFAdd(bc, ad, "")
			lhsval = b.CreateInsertValue(lhsval, real_, 0, "")
			result = b.CreateInsertValue(lhsval, imag_, 1, "")
		case token.ADD:
			real_ := b.CreateFAdd(a_, c_, "")
			imag_ := b.CreateFAdd(b_, d_, "")
			lhsval = b.CreateInsertValue(lhsval, real_, 0, "")
			result = b.CreateInsertValue(lhsval, imag_, 1, "")
		case token.SUB:
			real_ := b.CreateFSub(a_, c_, "")
			imag_ := b.CreateFSub(b_, d_, "")
			lhsval = b.CreateInsertValue(lhsval, real_, 0, "")
			result = b.CreateInsertValue(lhsval, imag_, 1, "")
		case token.EQL:
			realeq := b.CreateFCmp(llvm.FloatOEQ, a_, c_, "")
			imageq := b.CreateFCmp(llvm.FloatOEQ, b_, d_, "")
			result = b.CreateAnd(realeq, imageq, "")
		default:
			panic(fmt.Errorf("unhandled operator: %v", op))
		}
		return lhs.compiler.NewValue(result, lhs.typ)
	}

	// Floats and integers.
	// TODO determine the NaN rules.

	switch op {
	case token.MUL:
		if isFloat(lhs.typ) {
			result = b.CreateFMul(lhs.LLVMValue(), rhs.LLVMValue(), "")
		} else {
			result = b.CreateMul(lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.QUO:
		switch {
		case isFloat(lhs.typ):
			result = b.CreateFDiv(lhs.LLVMValue(), rhs.LLVMValue(), "")
		case !isUnsigned(lhs.typ):
			result = b.CreateSDiv(lhs.LLVMValue(), rhs.LLVMValue(), "")
		default:
			result = b.CreateUDiv(lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.REM:
		switch {
		case isFloat(lhs.typ):
			result = b.CreateFRem(lhs.LLVMValue(), rhs.LLVMValue(), "")
		case !isUnsigned(lhs.typ):
			result = b.CreateSRem(lhs.LLVMValue(), rhs.LLVMValue(), "")
		default:
			result = b.CreateURem(lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.ADD:
		if isFloat(lhs.typ) {
			result = b.CreateFAdd(lhs.LLVMValue(), rhs.LLVMValue(), "")
		} else {
			result = b.CreateAdd(lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.SUB:
		if isFloat(lhs.typ) {
			result = b.CreateFSub(lhs.LLVMValue(), rhs.LLVMValue(), "")
		} else {
			result = b.CreateSub(lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.SHL, token.SHR:
		return lhs.shift(rhs, op)
	case token.EQL:
		if isFloat(lhs.typ) {
			result = b.CreateFCmp(llvm.FloatOEQ, lhs.LLVMValue(), rhs.LLVMValue(), "")
		} else {
			result = b.CreateICmp(llvm.IntEQ, lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, types.Typ[types.Bool])
	case token.LSS:
		switch {
		case isFloat(lhs.typ):
			result = b.CreateFCmp(llvm.FloatOLT, lhs.LLVMValue(), rhs.LLVMValue(), "")
		case !isUnsigned(lhs.typ):
			result = b.CreateICmp(llvm.IntSLT, lhs.LLVMValue(), rhs.LLVMValue(), "")
		default:
			result = b.CreateICmp(llvm.IntULT, lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, types.Typ[types.Bool])
	case token.LEQ:
		switch {
		case isFloat(lhs.typ):
			result = b.CreateFCmp(llvm.FloatOLE, lhs.LLVMValue(), rhs.LLVMValue(), "")
		case !isUnsigned(lhs.typ):
			result = b.CreateICmp(llvm.IntSLE, lhs.LLVMValue(), rhs.LLVMValue(), "")
		default:
			result = b.CreateICmp(llvm.IntULE, lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, types.Typ[types.Bool])
	case token.GTR:
		switch {
		case isFloat(lhs.typ):
			result = b.CreateFCmp(llvm.FloatOGT, lhs.LLVMValue(), rhs.LLVMValue(), "")
		case !isUnsigned(lhs.typ):
			result = b.CreateICmp(llvm.IntSGT, lhs.LLVMValue(), rhs.LLVMValue(), "")
		default:
			result = b.CreateICmp(llvm.IntUGT, lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, types.Typ[types.Bool])
	case token.GEQ:
		switch {
		case isFloat(lhs.typ):
			result = b.CreateFCmp(llvm.FloatOGE, lhs.LLVMValue(), rhs.LLVMValue(), "")
		case !isUnsigned(lhs.typ):
			result = b.CreateICmp(llvm.IntSGE, lhs.LLVMValue(), rhs.LLVMValue(), "")
		default:
			result = b.CreateICmp(llvm.IntUGE, lhs.LLVMValue(), rhs.LLVMValue(), "")
		}
		return lhs.compiler.NewValue(result, types.Typ[types.Bool])
	case token.AND: // a & b
		result = b.CreateAnd(lhs.LLVMValue(), rhs.LLVMValue(), "")
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.AND_NOT: // a &^ b
		rhsval := rhs.LLVMValue()
		rhsval = b.CreateXor(rhsval, llvm.ConstAllOnes(rhsval.Type()), "")
		result = b.CreateAnd(lhs.LLVMValue(), rhsval, "")
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.OR: // a | b
		result = b.CreateOr(lhs.LLVMValue(), rhs.LLVMValue(), "")
		return lhs.compiler.NewValue(result, lhs.typ)
	case token.XOR: // a ^ b
		result = b.CreateXor(lhs.LLVMValue(), rhs.LLVMValue(), "")
		return lhs.compiler.NewValue(result, lhs.typ)
	default:
		panic(fmt.Sprint("Unimplemented operator: ", op))
	}
	panic("unreachable")
}

func (lhs *LLVMValue) shift(rhs *LLVMValue, op token.Token) *LLVMValue {
	rhs = rhs.Convert(lhs.Type()).(*LLVMValue)
	lhsval := lhs.LLVMValue()
	bits := rhs.LLVMValue()
	unsigned := isUnsigned(lhs.Type())
	b := lhs.compiler.builder
	// Shifting >= width of lhs yields undefined behaviour, so we must select.
	max := llvm.ConstInt(bits.Type(), uint64(lhsval.Type().IntTypeWidth()-1), false)
	var result llvm.Value
	if !unsigned && op == token.SHR {
		bits := b.CreateSelect(b.CreateICmp(llvm.IntULE, bits, max, ""), bits, max, "")
		result = b.CreateAShr(lhsval, bits, "")
	} else {
		if op == token.SHL {
			result = b.CreateShl(lhsval, bits, "")
		} else {
			result = b.CreateLShr(lhsval, bits, "")
		}
		zero := llvm.ConstNull(lhsval.Type())
		result = b.CreateSelect(b.CreateICmp(llvm.IntULE, bits, max, ""), result, zero, "")
	}
	return lhs.compiler.NewValue(result, lhs.typ)
}

func (v *LLVMValue) UnaryOp(op token.Token) Value {
	b := v.compiler.builder
	switch op {
	case token.SUB:
		var value llvm.Value
		if isFloat(v.typ) {
			zero := llvm.ConstNull(v.compiler.types.ToLLVM(v.Type()))
			value = b.CreateFSub(zero, v.LLVMValue(), "")
		} else {
			value = b.CreateNeg(v.LLVMValue(), "")
		}
		return v.compiler.NewValue(value, v.typ)
	case token.ADD:
		return v // No-op
	case token.NOT:
		value := b.CreateNot(v.LLVMValue(), "")
		return v.compiler.NewValue(value, v.typ)
	case token.XOR:
		lhs := v.LLVMValue()
		rhs := llvm.ConstAllOnes(lhs.Type())
		value := b.CreateXor(lhs, rhs, "")
		return v.compiler.NewValue(value, v.typ)
	default:
		panic(fmt.Sprintf("Unhandled operator: %s", op))
	}
	panic("unreachable")
}

func (v *LLVMValue) Convert(dsttyp types.Type) Value {
	b := v.compiler.builder

	// If it's a stack allocated value, we'll want to compare the
	// value type, not the pointer type.
	srctyp := v.typ

	// Get the underlying type, if any.
	origdsttyp := dsttyp
	dsttyp = dsttyp.Underlying()
	srctyp = srctyp.Underlying()

	// Identical (underlying) types? Just swap in the destination type.
	if types.IsIdentical(srctyp, dsttyp) {
		return v.compiler.NewValue(v.LLVMValue(), origdsttyp)
	}

	// Both pointer types with identical underlying types? Same as above.
	if srctyp, ok := srctyp.(*types.Pointer); ok {
		if dsttyp, ok := dsttyp.(*types.Pointer); ok {
			srctyp := srctyp.Elem().Underlying()
			dsttyp := dsttyp.Elem().Underlying()
			if types.IsIdentical(srctyp, dsttyp) {
				return v.compiler.NewValue(v.LLVMValue(), origdsttyp)
			}
		}
	}

	byteslice := types.NewSlice(types.Typ[types.Byte])
	runeslice := types.NewSlice(types.Typ[types.Rune])

	// string ->
	if isString(srctyp) {
		// (untyped) string -> string
		// XXX should untyped strings be able to escape go/types?
		if isString(dsttyp) {
			return v.compiler.NewValue(v.LLVMValue(), origdsttyp)
		}

		// string -> []byte
		if types.IsIdentical(dsttyp, byteslice) {
			c := v.compiler
			value := v.LLVMValue()
			strdata := c.builder.CreateExtractValue(value, 0, "")
			strlen := c.builder.CreateExtractValue(value, 1, "")

			// Data must be copied, to prevent changes in
			// the byte slice from mutating the string.
			newdata := c.createMalloc(strlen)
			memcpy := c.runtime.memcpy.LLVMValue()
			c.builder.CreateCall(memcpy, []llvm.Value{
				newdata,
				c.builder.CreatePtrToInt(strdata, c.target.IntPtrType(), ""),
				strlen,
			}, "")
			strdata = c.builder.CreateIntToPtr(newdata, strdata.Type(), "")

			struct_ := llvm.Undef(c.types.ToLLVM(byteslice))
			struct_ = c.builder.CreateInsertValue(struct_, strdata, 0, "")
			struct_ = c.builder.CreateInsertValue(struct_, strlen, 1, "")
			struct_ = c.builder.CreateInsertValue(struct_, strlen, 2, "")
			return c.NewValue(struct_, byteslice)
		}

		// string -> []rune
		if types.IsIdentical(dsttyp, runeslice) {
			return v.stringToRuneSlice()
		}
	}

	// []byte -> string
	if types.IsIdentical(srctyp, byteslice) && isString(dsttyp) {
		c := v.compiler
		value := v.LLVMValue()
		data := c.builder.CreateExtractValue(value, 0, "")
		len := c.builder.CreateExtractValue(value, 1, "")

		// Data must be copied, to prevent changes in
		// the byte slice from mutating the string.
		newdata := c.createMalloc(len)
		memcpy := c.runtime.memcpy.LLVMValue()
		c.builder.CreateCall(memcpy, []llvm.Value{
			newdata,
			c.builder.CreatePtrToInt(data, c.target.IntPtrType(), ""),
			len,
		}, "")
		data = c.builder.CreateIntToPtr(newdata, data.Type(), "")

		struct_ := llvm.Undef(c.types.ToLLVM(types.Typ[types.String]))
		struct_ = c.builder.CreateInsertValue(struct_, data, 0, "")
		struct_ = c.builder.CreateInsertValue(struct_, len, 1, "")
		return c.NewValue(struct_, types.Typ[types.String])
	}

	// []rune -> string
	if types.IsIdentical(srctyp, runeslice) && isString(dsttyp) {
		return v.runeSliceToString()
	}

	// rune -> string
	if isString(dsttyp) && isInteger(srctyp) {
		return v.runeToString()
	}

	// TODO other special conversions?
	llvm_type := v.compiler.types.ToLLVM(dsttyp)

	// Unsafe pointer conversions.
	if dsttyp == types.Typ[types.UnsafePointer] { // X -> unsafe.Pointer
		if _, isptr := srctyp.(*types.Pointer); isptr {
			value := b.CreatePtrToInt(v.LLVMValue(), llvm_type, "")
			return v.compiler.NewValue(value, origdsttyp)
		} else if srctyp == types.Typ[types.Uintptr] {
			return v.compiler.NewValue(v.LLVMValue(), origdsttyp)
		}
	} else if srctyp == types.Typ[types.UnsafePointer] { // unsafe.Pointer -> X
		if _, isptr := dsttyp.(*types.Pointer); isptr {
			value := b.CreateIntToPtr(v.LLVMValue(), llvm_type, "")
			return v.compiler.NewValue(value, origdsttyp)
		} else if dsttyp == types.Typ[types.Uintptr] {
			return v.compiler.NewValue(v.LLVMValue(), origdsttyp)
		}
	}

	lv := v.LLVMValue()
	srcType := lv.Type()
	switch srcType.TypeKind() {
	case llvm.IntegerTypeKind:
		switch llvm_type.TypeKind() {
		case llvm.IntegerTypeKind:
			srcBits := srcType.IntTypeWidth()
			dstBits := llvm_type.IntTypeWidth()
			delta := srcBits - dstBits
			switch {
			case delta < 0:
				if !isUnsigned(srctyp) {
					lv = b.CreateSExt(lv, llvm_type, "")
				} else {
					lv = b.CreateZExt(lv, llvm_type, "")
				}
			case delta > 0:
				lv = b.CreateTrunc(lv, llvm_type, "")
			}
			return v.compiler.NewValue(lv, origdsttyp)
		case llvm.FloatTypeKind, llvm.DoubleTypeKind:
			if !isUnsigned(v.Type()) {
				lv = b.CreateSIToFP(lv, llvm_type, "")
			} else {
				lv = b.CreateUIToFP(lv, llvm_type, "")
			}
			return v.compiler.NewValue(lv, origdsttyp)
		}
	case llvm.DoubleTypeKind:
		switch llvm_type.TypeKind() {
		case llvm.FloatTypeKind:
			lv = b.CreateFPTrunc(lv, llvm_type, "")
			return v.compiler.NewValue(lv, origdsttyp)
		case llvm.IntegerTypeKind:
			if !isUnsigned(dsttyp) {
				lv = b.CreateFPToSI(lv, llvm_type, "")
			} else {
				lv = b.CreateFPToUI(lv, llvm_type, "")
			}
			return v.compiler.NewValue(lv, origdsttyp)
		}
	case llvm.FloatTypeKind:
		switch llvm_type.TypeKind() {
		case llvm.DoubleTypeKind:
			lv = b.CreateFPExt(lv, llvm_type, "")
			return v.compiler.NewValue(lv, origdsttyp)
		case llvm.IntegerTypeKind:
			if !isUnsigned(dsttyp) {
				lv = b.CreateFPToSI(lv, llvm_type, "")
			} else {
				lv = b.CreateFPToUI(lv, llvm_type, "")
			}
			return v.compiler.NewValue(lv, origdsttyp)
		}
	}

	// Complex -> complex. Complexes are only convertible to other
	// complexes, contant conversions aside. So we can just check the
	// source type here; given that the types are not identical
	// (checked above), we can assume the destination type is the alternate
	// complex type.
	if isComplex(srctyp) {
		var fpcast func(llvm.Builder, llvm.Value, llvm.Type, string) llvm.Value
		var fptype llvm.Type
		if srctyp == types.Typ[types.Complex64] {
			fpcast = (llvm.Builder).CreateFPExt
			fptype = llvm.DoubleType()
		} else {
			fpcast = (llvm.Builder).CreateFPTrunc
			fptype = llvm.FloatType()
		}
		if fpcast != nil {
			realv := b.CreateExtractValue(lv, 0, "")
			imagv := b.CreateExtractValue(lv, 1, "")
			realv = fpcast(b, realv, fptype, "")
			imagv = fpcast(b, imagv, fptype, "")
			lv = llvm.Undef(v.compiler.types.ToLLVM(dsttyp))
			lv = b.CreateInsertValue(lv, realv, 0, "")
			lv = b.CreateInsertValue(lv, imagv, 1, "")
			return v.compiler.NewValue(lv, origdsttyp)
		}
	}
	panic(fmt.Sprintf("unimplemented conversion: %s (%s) -> %s", v.typ, lv.Type(), origdsttyp))
}

func (v *LLVMValue) LLVMValue() llvm.Value {
	return v.value
}

func (v *LLVMValue) Type() types.Type {
	return v.typ
}

func (v *LLVMValue) extractComplexComponent(index int) *LLVMValue {
	value := v.LLVMValue()
	component := v.compiler.builder.CreateExtractValue(value, index, "")
	if component.Type().TypeKind() == llvm.FloatTypeKind {
		return v.compiler.NewValue(component, types.Typ[types.Float32])
	}
	return v.compiler.NewValue(component, types.Typ[types.Float64])
}

func boolLLVMValue(v bool) (lv llvm.Value) {
	if v {
		lv = llvm.ConstAllOnes(llvm.Int1Type())
	} else {
		lv = llvm.ConstNull(llvm.Int1Type())
	}
	return lv
}
