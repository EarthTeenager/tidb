// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"go.uber.org/zap"
)

// specialFoldHandler stores functions for special UDF to constant fold
var specialFoldHandler = map[string]func(*ScalarFunction) (Expression, bool){}

func init() {
	specialFoldHandler = map[string]func(*ScalarFunction) (Expression, bool){
		ast.If:     ifFoldHandler,
		ast.Ifnull: ifNullFoldHandler,
		ast.Case:   caseWhenHandler,
		ast.IsNull: isNullHandler,
	}
}

// FoldConstant does constant folding optimization on an expression excluding deferred ones.
func FoldConstant(expr Expression) Expression {
	e, _ := foldConstant(expr)
	// keep the original coercibility, charset, collation and repertoire values after folding
	e.SetCoercibility(expr.Coercibility())

	charset, collate := expr.GetType().GetCharset(), expr.GetType().GetCollate()
	e.GetType().SetCharset(charset)
	e.GetType().SetCollate(collate)
	e.SetRepertoire(expr.Repertoire())
	return e
}

func isNullHandler(expr *ScalarFunction) (Expression, bool) {
	arg0 := expr.GetArgs()[0]
	if constArg, isConst := arg0.(*Constant); isConst {
		isDeferredConst := constArg.DeferredExpr != nil || constArg.ParamMarker != nil
		value, err := expr.Eval(chunk.Row{})
		if err != nil {
			// Failed to fold this expr to a constant, print the DEBUG log and
			// return the original expression to let the error to be evaluated
			// again, in that time, the error is returned to the client.
			logutil.BgLogger().Debug("fold expression to constant", zap.String("expression", expr.ExplainInfo()), zap.Error(err))
			return expr, isDeferredConst
		}
		if isDeferredConst {
			return &Constant{Value: value, RetType: expr.RetType, DeferredExpr: expr}, true
		}
		return &Constant{Value: value, RetType: expr.RetType}, false
	}
	if mysql.HasNotNullFlag(arg0.GetType().GetFlag()) {
		return NewZero(), false
	}
	return expr, false
}

func ifFoldHandler(expr *ScalarFunction) (Expression, bool) {
	args := expr.GetArgs()
	foldedArg0, _ := foldConstant(args[0])
	if constArg, isConst := foldedArg0.(*Constant); isConst {
		arg0, isNull0, err := constArg.EvalInt(expr.Function.getCtx(), chunk.Row{})
		if err != nil {
			// Failed to fold this expr to a constant, print the DEBUG log and
			// return the original expression to let the error to be evaluated
			// again, in that time, the error is returned to the client.
			logutil.BgLogger().Debug("fold expression to constant", zap.String("expression", expr.ExplainInfo()), zap.Error(err))
			return expr, false
		}
		if !isNull0 && arg0 != 0 {
			return foldConstant(args[1])
		}
		return foldConstant(args[2])
	}
	// if the condition is not const, which branch is unknown to run, so directly return.
	return expr, false
}

func ifNullFoldHandler(expr *ScalarFunction) (Expression, bool) {
	args := expr.GetArgs()
	foldedArg0, isDeferred := foldConstant(args[0])
	if constArg, isConst := foldedArg0.(*Constant); isConst {
		// Only check constArg.Value here. Because deferred expression is
		// evaluated to constArg.Value after foldConstant(args[0]), it's not
		// needed to be checked.
		if constArg.Value.IsNull() {
			return foldConstant(args[1])
		}
		return constArg, isDeferred
	}
	// if the condition is not const, which branch is unknown to run, so directly return.
	return expr, false
}

func caseWhenHandler(expr *ScalarFunction) (Expression, bool) {
	args, l := expr.GetArgs(), len(expr.GetArgs())
	var isDeferred, isDeferredConst bool
	for i := 0; i < l-1; i += 2 {
		expr.GetArgs()[i], isDeferred = foldConstant(args[i])
		isDeferredConst = isDeferredConst || isDeferred
		if _, isConst := expr.GetArgs()[i].(*Constant); !isConst {
			// for no-const, here should return directly, because the following branches are unknown to be run or not
			return expr, false
		}
		// If the condition is const and true, and the previous conditions
		// has no expr, then the folded execution body is returned, otherwise
		// the arguments of the casewhen are folded and replaced.
		val, isNull, err := args[i].EvalInt(expr.GetCtx(), chunk.Row{})
		if err != nil {
			return expr, false
		}
		if val != 0 && !isNull {
			foldedExpr, isDeferred := foldConstant(args[i+1])
			isDeferredConst = isDeferredConst || isDeferred
			if _, isConst := foldedExpr.(*Constant); isConst {
				foldedExpr.GetType().SetDecimal(expr.GetType().GetDecimal())
				return foldedExpr, isDeferredConst
			}
			return foldedExpr, isDeferredConst
		}
	}
	// If the number of arguments in casewhen is odd, and the previous conditions
	// is false, then the folded else execution body is returned. otherwise
	// the execution body of the else are folded and replaced.
	if l%2 == 1 {
		foldedExpr, isDeferred := foldConstant(args[l-1])
		isDeferredConst = isDeferredConst || isDeferred
		if _, isConst := foldedExpr.(*Constant); isConst {
			foldedExpr.GetType().SetDecimal(expr.GetType().GetDecimal())
			return foldedExpr, isDeferredConst
		}
		return foldedExpr, isDeferredConst
	}
	return expr, isDeferredConst
}

func foldConstant(expr Expression) (Expression, bool) {
	switch x := expr.(type) {
	case *ScalarFunction:
		if _, ok := unFoldableFunctions[x.FuncName.L]; ok {
			return expr, false
		}
		if function := specialFoldHandler[x.FuncName.L]; function != nil && !MaybeOverOptimized4PlanCache(x.GetCtx(), []Expression{expr}) {
			return function(x)
		}

		args := x.GetArgs()
		sc := x.GetCtx().GetSessionVars().StmtCtx
		argIsConst := make([]bool, len(args))
		hasNullArg := false
		allConstArg := true
		isDeferredConst := false
		for i := 0; i < len(args); i++ {
			switch x := args[i].(type) {
			case *Constant:
				isDeferredConst = isDeferredConst || x.DeferredExpr != nil || x.ParamMarker != nil
				argIsConst[i] = true
				hasNullArg = hasNullArg || x.Value.IsNull()
			default:
				allConstArg = false
			}
		}
		if !allConstArg {
			// try to optimize on the situation when not all arguments are const
			// for most functions, if one of the arguments are NULL, the result can be a constant (NULL or something else)
			//
			// NullEQ and ConcatWS are excluded, because they could have different value when the non-constant value is
			// 1 or NULL. For example, concat_ws(NULL, NULL) gives NULL, but concat_ws(1, NULL) gives ''
			if !hasNullArg || !sc.InNullRejectCheck || x.FuncName.L == ast.NullEQ || x.FuncName.L == ast.ConcatWS {
				return expr, isDeferredConst
			}
			constArgs := make([]Expression, len(args))
			for i, arg := range args {
				if argIsConst[i] {
					constArgs[i] = arg
				} else {
					constArgs[i] = NewOne()
				}
			}
			dummyScalarFunc, err := NewFunctionBase(x.GetCtx(), x.FuncName.L, x.GetType(), constArgs...)
			if err != nil {
				return expr, isDeferredConst
			}
			value, err := dummyScalarFunc.Eval(chunk.Row{})
			if err != nil {
				return expr, isDeferredConst
			}
			if value.IsNull() {
				// This Constant is created to compose the result expression of EvaluateExprWithNull when InNullRejectCheck
				// is true. We just check whether the result expression is null or false and then let it die. Basically,
				// the constant is used once briefly and will not be retained for a long time. Hence setting DeferredExpr
				// of Constant to nil is ok.
				return &Constant{Value: value, RetType: x.RetType}, false
			}
			if isTrue, err := value.ToBool(sc.TypeCtx()); err == nil && isTrue == 0 {
				// This Constant is created to compose the result expression of EvaluateExprWithNull when InNullRejectCheck
				// is true. We just check whether the result expression is null or false and then let it die. Basically,
				// the constant is used once briefly and will not be retained for a long time. Hence setting DeferredExpr
				// of Constant to nil is ok.
				return &Constant{Value: value, RetType: x.RetType}, false
			}
			return expr, isDeferredConst
		}
		value, err := x.Eval(chunk.Row{})
		retType := x.RetType.Clone()
		if !hasNullArg {
			// set right not null flag for constant value
			switch value.Kind() {
			case types.KindNull:
				retType.DelFlag(mysql.NotNullFlag)
			default:
				retType.AddFlag(mysql.NotNullFlag)
			}
		}
		if err != nil {
			logutil.BgLogger().Debug("fold expression to constant", zap.String("expression", x.ExplainInfo()), zap.Error(err))
			return expr, isDeferredConst
		}
		if isDeferredConst {
			return &Constant{Value: value, RetType: retType, DeferredExpr: x}, true
		}
		return &Constant{Value: value, RetType: retType}, false
	case *Constant:
		if x.ParamMarker != nil {
			return &Constant{
				Value:        x.ParamMarker.GetUserVar(),
				RetType:      x.RetType,
				DeferredExpr: x.DeferredExpr,
				ParamMarker:  x.ParamMarker,
			}, true
		} else if x.DeferredExpr != nil {
			value, err := x.DeferredExpr.Eval(chunk.Row{})
			if err != nil {
				logutil.BgLogger().Debug("fold expression to constant", zap.String("expression", x.ExplainInfo()), zap.Error(err))
				return expr, true
			}
			return &Constant{Value: value, RetType: x.RetType, DeferredExpr: x.DeferredExpr}, true
		}
	}
	return expr, false
}
