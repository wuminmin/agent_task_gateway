package queryplan

import "testing"

func TestNormalizeArithmeticCanonical(t *testing.T) {
	price := ArithOperand{Column: "sales.price", SQLType: "numeric"}
	qty := ArithOperand{Column: "sales.qty", SQLType: "numeric"}
	forward, err := NormalizeArithmetic(&DerivedExpr{Op: ArithMul, SQLType: "numeric", Operands: []ArithOperand{price, qty}})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := NormalizeArithmetic(&DerivedExpr{Op: ArithMul, SQLType: "numeric", Operands: []ArithOperand{qty, price}})
	if err != nil {
		t.Fatal(err)
	}
	if forward != reversed || forward != "mul(sales.price,sales.qty)" {
		t.Fatalf("commutative canonicalization broken: %q vs %q", forward, reversed)
	}
	subForward, err := NormalizeArithmetic(&DerivedExpr{Op: ArithSub, SQLType: "numeric", Operands: []ArithOperand{price, qty}})
	if err != nil {
		t.Fatal(err)
	}
	subReversed, err := NormalizeArithmetic(&DerivedExpr{Op: ArithSub, SQLType: "numeric", Operands: []ArithOperand{qty, price}})
	if err != nil {
		t.Fatal(err)
	}
	if subForward == subReversed {
		t.Fatal("subtraction must keep operand order in its identity")
	}
	nested, err := NormalizeArithmetic(&DerivedExpr{Op: ArithMul, SQLType: "numeric", Operands: []ArithOperand{
		price,
		{Nested: &DerivedExpr{Op: ArithSub, SQLType: "numeric", Operands: []ArithOperand{
			{Literal: "1", SQLType: "numeric"}, {Column: "sales.discount", SQLType: "numeric"},
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if nested != "mul(sales.price,sub(lit(numeric):1,sales.discount))" {
		t.Fatalf("nested canonical form = %q", nested)
	}
}

func TestNormalizeArithmeticFailsClosed(t *testing.T) {
	price := ArithOperand{Column: "sales.price", SQLType: "numeric"}
	cases := []*DerivedExpr{
		{Op: "mod", SQLType: "numeric", Operands: []ArithOperand{price, price}},
		{Op: ArithAdd, SQLType: "double precision", Operands: []ArithOperand{price, price}},
		{Op: ArithAdd, SQLType: "numeric", Operands: []ArithOperand{price}},
		{Op: ArithAdd, SQLType: "numeric", Operands: []ArithOperand{price, {Column: "no-namespace", SQLType: "numeric"}}},
		{Op: ArithAdd, SQLType: "numeric", Operands: []ArithOperand{price, {Literal: "1 or 2", SQLType: "numeric"}}},
		{Op: ArithAdd, SQLType: "numeric", Operands: []ArithOperand{price, {}}},
	}
	for index, expr := range cases {
		if _, err := NormalizeArithmetic(expr); err == nil {
			t.Fatalf("case %d must fail closed", index)
		}
	}
	deep := &DerivedExpr{Op: ArithAdd, SQLType: "numeric", Operands: []ArithOperand{price, price}}
	for i := 0; i < 5; i++ {
		deep = &DerivedExpr{Op: ArithAdd, SQLType: "numeric", Operands: []ArithOperand{price, {Nested: deep}}}
	}
	if _, err := NormalizeArithmetic(deep); err == nil {
		t.Fatal("depth bound must fail closed")
	}
}
