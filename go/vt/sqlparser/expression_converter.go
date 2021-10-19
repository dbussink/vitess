/*
Copyright 2020 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sqlparser

import (
	"fmt"

	"vitess.io/vitess/go/vt/vtgate/evalengine"
)

var ErrConvertExprNotSupported = "expr cannot be converted, not supported"

//Convert converts between AST expressions and executable expressions
func Convert(e Expr, columnLookup func(col *ColName) (int, error)) (evalengine.Expr, error) {
	switch node := e.(type) {
	case *ColName:
		if columnLookup == nil {
			break
		}
		idx, err := columnLookup(node)
		if err != nil {
			return nil, err
		}
		return &evalengine.Column{Offset: idx}, nil
	case *ComparisonExpr:
		if node.Operator != EqualOp {
			return nil, fmt.Errorf("%s: %T with %s", ErrConvertExprNotSupported, node, node.Operator.ToString())
		}
		left, err := Convert(node.Left, columnLookup)
		if err != nil {
			return nil, err
		}
		right, err := Convert(node.Right, columnLookup)
		if err != nil {
			return nil, err
		}
		return &evalengine.Equals{
			Left:  left,
			Right: right,
		}, nil
	case Argument:
		return evalengine.NewBindVar(string(node)), nil
	case *Literal:
		switch node.Type {
		case IntVal:
			return evalengine.NewLiteralIntFromBytes(node.Bytes())
		case FloatVal:
			return evalengine.NewLiteralFloatFromBytes(node.Bytes())
		case StrVal:
			return evalengine.NewLiteralString(node.Bytes()), nil
		}
	case BoolVal:
		if node {
			return evalengine.NewLiteralIntFromBytes([]byte("1"))
		}
		return evalengine.NewLiteralIntFromBytes([]byte("0"))
	case *BinaryExpr:
		var op evalengine.BinaryExpr
		switch node.Operator {
		case PlusOp:
			op = &evalengine.Addition{}
		case MinusOp:
			op = &evalengine.Subtraction{}
		case MultOp:
			op = &evalengine.Multiplication{}
		case DivOp:
			op = &evalengine.Division{}
		default:
			return nil, fmt.Errorf("%s: %T", ErrConvertExprNotSupported, e)
		}
		left, err := Convert(node.Left, columnLookup)
		if err != nil {
			return nil, err
		}
		right, err := Convert(node.Right, columnLookup)
		if err != nil {
			return nil, err
		}
		return &evalengine.BinaryOp{
			Expr:  op,
			Left:  left,
			Right: right,
		}, nil

	}
	return nil, fmt.Errorf("%s: %T", ErrConvertExprNotSupported, e)
}
