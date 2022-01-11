/*
Copyright 2021 The Vitess Authors.

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

package planbuilder

import (
	"fmt"
	"io"

	"vitess.io/vitess/go/vt/vtgate/planbuilder/context"

	"vitess.io/vitess/go/vt/vtgate/planbuilder/physical"

	"vitess.io/vitess/go/vt/log"

	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/vt/vtgate/evalengine"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/abstract"
	"vitess.io/vitess/go/vt/vtgate/semantics"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
)

var verboseLogging = false

type (
	opCacheMap map[tableSetPair]abstract.PhysicalOperator
)

func createPhysicalOperator(ctx *context.PlanningContext, opTree abstract.LogicalOperator) (abstract.PhysicalOperator, error) {
	switch op := opTree.(type) {
	case *abstract.QueryGraph:
		switch {
		// case ctx.vschema.Planner() == Gen4Left2Right:
		//	return leftToRightSolve(ctx, op)
		default:
			return greedySolve2(ctx, op)
		}
	case *abstract.Join:
		opInner, err := createPhysicalOperator(ctx, op.LHS)
		if err != nil {
			return nil, err
		}
		opOuter, err := createPhysicalOperator(ctx, op.RHS)
		if err != nil {
			return nil, err
		}
		return mergeOrJoinOp(ctx, opInner, opOuter, sqlparser.SplitAndExpression(nil, op.Predicate), !op.LeftJoin)
	// case *abstract.Derived:
	//	treeInner, err := optimizeQuery(ctx, op.Inner)
	//	if err != nil {
	//		return nil, err
	//	}
	//	return &derivedTree{
	//		query:         op.Sel,
	//		inner:         treeInner,
	//		alias:         op.Alias,
	//		columnAliases: op.ColumnAliases,
	//	}, nil
	case *abstract.SubQuery:
		return optimizeSubQueryOp(ctx, op)
	case *abstract.Vindex:
		return optimizeVindexOp(ctx, op)
	case *abstract.Concatenate:
		return optimizeUnionOp(ctx, op)
	default:
		return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid operator tree: %T", op)
	}
}

/*
	The greedy planner will plan a query by finding first finding the best route plan for every table.
    Then, iteratively, it finds the cheapest join that can be produced between the remaining plans,
	and removes the two inputs to this cheapest plan and instead adds the join.
	As an optimization, it first only considers joining tables that have predicates defined between them
*/
func greedySolve2(ctx *context.PlanningContext, qg *abstract.QueryGraph) (abstract.PhysicalOperator, error) {
	routeOps, err := seedOperatorList(ctx, qg)
	planCache := opCacheMap{}
	if err != nil {
		return nil, err
	}

	op, err := mergeRouteOps(ctx, qg, routeOps, planCache, false)
	if err != nil {
		return nil, err
	}
	return op, nil
}

// seedOperatorList returns a route for each table in the qg
func seedOperatorList(ctx *context.PlanningContext, qg *abstract.QueryGraph) ([]abstract.PhysicalOperator, error) {
	plans := make([]abstract.PhysicalOperator, len(qg.Tables))

	// we start by seeding the table with the single routes
	for i, table := range qg.Tables {
		solves := ctx.SemTable.TableSetFor(table.Alias)
		plan, err := createRouteOperator(ctx, table, solves)
		if err != nil {
			return nil, err
		}
		// if qg.NoDeps != nil {
		//	plan.predicates = append(plan.predicates, sqlparser.SplitAndExpression(nil, qg.NoDeps)...)
		// }
		plans[i] = plan
	}
	return plans, nil
}

func createRouteOperator(ctx *context.PlanningContext, table *abstract.QueryTable, solves semantics.TableSet) (*routeOp, error) {
	// if table.IsInfSchema {
	//	ks, err := ctx.vschema.AnyKeyspace()
	//	if err != nil {
	//		return nil, err
	//	}
	//	rp := &routeTree{
	//		routeOpCode: engine.SelectDBA,
	//		solved:      solves,
	//		keyspace:    ks,
	//		tables: []relation{&routeTable{
	//			qtable: table,
	//			vtable: &vindexes.Table{
	//				Name:     table.Table.Name,
	//				Keyspace: ks,
	//			},
	//		}},
	//		predicates: table.Predicates,
	//	}
	//	err = rp.findSysInfoRoutingPredicatesGen4(ctx.ReservedVars)
	//	if err != nil {
	//		return nil, err
	//	}
	//
	//	return rp, nil
	// }
	vschemaTable, _, _, _, _, err := ctx.VSchema.FindTableOrVindex(table.Table)
	if err != nil {
		return nil, err
	}
	if vschemaTable.Name.String() != table.Table.Name.String() {
		// we are dealing with a routed table
		name := table.Table.Name
		table.Table.Name = vschemaTable.Name
		astTable, ok := table.Alias.Expr.(sqlparser.TableName)
		if !ok {
			return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] a derived table should never be a routed table")
		}
		realTableName := sqlparser.NewTableIdent(vschemaTable.Name.String())
		astTable.Name = realTableName
		if table.Alias.As.IsEmpty() {
			// if the user hasn't specified an alias, we'll insert one here so the old table name still works
			table.Alias.As = sqlparser.NewTableIdent(name.String())
		}
	}
	plan := &routeOp{
		source: &physical.TableOp{
			QTable: table,
			VTable: vschemaTable,
		},
		keyspace: vschemaTable.Keyspace,
	}

	for _, columnVindex := range vschemaTable.ColumnVindexes {
		plan.vindexPreds = append(plan.vindexPreds, &vindexPlusPredicates{colVindex: columnVindex, tableID: solves})
	}

	switch {
	case vschemaTable.Type == vindexes.TypeSequence:
		plan.routeOpCode = engine.SelectNext
	case vschemaTable.Type == vindexes.TypeReference:
		plan.routeOpCode = engine.SelectReference
	case !vschemaTable.Keyspace.Sharded:
		plan.routeOpCode = engine.SelectUnsharded
	case vschemaTable.Pinned != nil:
		// Pinned tables have their keyspace ids already assigned.
		// Use the Binary vindex, which is the identity function
		// for keyspace id.
		plan.routeOpCode = engine.SelectEqualUnique
		vindex, _ := vindexes.NewBinary("binary", nil)
		plan.selected = &vindexOption{
			ready:       true,
			values:      []evalengine.Expr{evalengine.NewLiteralString(vschemaTable.Pinned, collations.TypedCollation{})},
			valueExprs:  nil,
			predicates:  nil,
			opcode:      engine.SelectEqualUnique,
			foundVindex: vindex,
			cost: cost{
				opCode: engine.SelectEqualUnique,
			},
		}
	default:
		plan.routeOpCode = engine.SelectScatter
	}
	for _, predicate := range table.Predicates {
		err = plan.updateRoutingLogic(ctx, predicate)
		if err != nil {
			return nil, err
		}
	}

	return plan, nil
}

func mergeRouteOps(ctx *context.PlanningContext, qg *abstract.QueryGraph, physicalOps []abstract.PhysicalOperator, planCache opCacheMap, crossJoinsOK bool) (abstract.PhysicalOperator, error) {
	if len(physicalOps) == 0 {
		return nil, nil
	}
	for len(physicalOps) > 1 {
		bestTree, lIdx, rIdx, err := findBestJoinOp(ctx, qg, physicalOps, planCache, crossJoinsOK)
		if err != nil {
			return nil, err
		}
		// if we found a plan, we'll replace the two plans that were joined with the join plan created
		if bestTree != nil {
			// we remove one plan, and replace the other
			if rIdx > lIdx {
				physicalOps = removeOpAt(physicalOps, rIdx)
				physicalOps = removeOpAt(physicalOps, lIdx)
			} else {
				physicalOps = removeOpAt(physicalOps, lIdx)
				physicalOps = removeOpAt(physicalOps, rIdx)
			}
			physicalOps = append(physicalOps, bestTree)
		} else {
			if crossJoinsOK {
				return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "should not happen")
			}
			// we will only fail to find a join plan when there are only cross joins left
			// when that happens, we switch over to allow cross joins as well.
			// this way we prioritize joining physicalOps with predicates first
			crossJoinsOK = true
		}
	}
	return physicalOps[0], nil
}

func removeOpAt(plans []abstract.PhysicalOperator, idx int) []abstract.PhysicalOperator {
	return append(plans[:idx], plans[idx+1:]...)
}

func findBestJoinOp(
	ctx *context.PlanningContext,
	qg *abstract.QueryGraph,
	plans []abstract.PhysicalOperator,
	planCache opCacheMap,
	crossJoinsOK bool,
) (bestPlan abstract.PhysicalOperator, lIdx int, rIdx int, err error) {
	for i, lhs := range plans {
		for j, rhs := range plans {
			if i == j {
				continue
			}
			joinPredicates := qg.GetPredicates(lhs.TableID(), rhs.TableID())
			if len(joinPredicates) == 0 && !crossJoinsOK {
				// if there are no predicates joining the two tables,
				// creating a join between them would produce a
				// cartesian product, which is almost always a bad idea
				continue
			}
			plan, err := getJoinOpFor(ctx, planCache, lhs, rhs, joinPredicates)
			if err != nil {
				return nil, 0, 0, err
			}
			if bestPlan == nil || plan.Cost() < bestPlan.Cost() {
				if verboseLogging {
					log.Warningf("New Best Plan - %v and cost - %d", plan.TableID(), plan.Cost())
					switch node := plan.(type) {
					case *physical.ApplyJoin:
						log.Warningf("Join Plan - lhs - %v, rhs - %v", node.LHS.TableID(), node.RHS.TableID())
					case *routeOp:
						joinOp := node.source.(*physical.ApplyJoin)
						log.Warningf("Route Plan - lhs - %v, rhs - %v", joinOp.LHS.TableID(), joinOp.RHS.TableID())
					}
				}
				bestPlan = plan
				// remember which plans we based on, so we can remove them later
				lIdx = i
				rIdx = j
			}
		}
	}
	return bestPlan, lIdx, rIdx, nil
}

func getJoinOpFor(ctx *context.PlanningContext, cm opCacheMap, lhs, rhs abstract.PhysicalOperator, joinPredicates []sqlparser.Expr) (abstract.PhysicalOperator, error) {
	solves := tableSetPair{left: lhs.TableID(), right: rhs.TableID()}
	cachedPlan := cm[solves]
	if cachedPlan != nil {
		return cachedPlan, nil
	}

	join, err := mergeOrJoinOp(ctx, lhs, rhs, joinPredicates, true)
	if err != nil {
		return nil, err
	}
	cm[solves] = join
	return join, nil
}

func mergeOrJoinOp(ctx *context.PlanningContext, lhs, rhs abstract.PhysicalOperator, joinPredicates []sqlparser.Expr, inner bool) (abstract.PhysicalOperator, error) {

	merger := func(a, b *routeOp) (*routeOp, error) {
		return createRouteOperatorForJoin(ctx, a, b, joinPredicates, inner)
	}

	newPlan, _ := tryMergeOp(ctx, lhs, rhs, joinPredicates, merger)
	if newPlan != nil {
		return newPlan, nil
	}

	var tree abstract.PhysicalOperator = &physical.ApplyJoin{
		LHS:      lhs.Clone(),
		RHS:      rhs.Clone(),
		Vars:     map[string]int{},
		LeftJoin: !inner,
	}
	for _, predicate := range joinPredicates {
		var err error
		tree, err = PushPredicate(ctx, predicate, tree)
		if err != nil {
			return nil, err
		}
	}
	return tree, nil
}

func createRouteOperatorForJoin(ctx *context.PlanningContext, aRoute, bRoute *routeOp, joinPredicates []sqlparser.Expr, inner bool) (*routeOp, error) {
	// append system table names from both the routes.
	sysTableName := aRoute.SysTableTableName
	if sysTableName == nil {
		sysTableName = bRoute.SysTableTableName
	} else {
		for k, v := range bRoute.SysTableTableName {
			sysTableName[k] = v
		}
	}

	r := &routeOp{
		routeOpCode:         aRoute.routeOpCode,
		keyspace:            aRoute.keyspace,
		vindexPreds:         append(aRoute.vindexPreds, bRoute.vindexPreds...),
		SysTableTableSchema: append(aRoute.SysTableTableSchema, bRoute.SysTableTableSchema...),
		SysTableTableName:   sysTableName,
		source: &physical.ApplyJoin{
			LHS:      aRoute.source,
			RHS:      bRoute.source,
			Vars:     map[string]int{},
			LeftJoin: !inner,
		},
	}

	for _, predicate := range joinPredicates {
		op, err := PushPredicate(ctx, predicate, r)
		if err != nil {
			return nil, err
		}
		route, ok := op.(*routeOp)
		if !ok {
			return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] did not expect type to change when pushing predicates")
		}
		r = route
	}

	if aRoute.selectedVindex() == bRoute.selectedVindex() {
		r.selected = aRoute.selected
	}

	return r, nil
}

type mergeOpFunc func(a, b *routeOp) (*routeOp, error)

func makeRouteOp(j abstract.PhysicalOperator) *routeOp {
	rb, ok := j.(*routeOp)
	if ok {
		return rb
	}

	return nil

	// x, ok := j.(*derivedTree)
	// if !ok {
	//	return nil
	// }
	// dp := x.Clone().(*derivedTree)
	//
	// inner := makeRouteOp(dp.inner)
	// if inner == nil {
	//	return nil
	// }

	// dt := &derivedTable{
	//	tables:     inner.tables,
	//	query:      dp.query,
	//	predicates: inner.predicates,
	//	leftJoins:  inner.leftJoins,
	//	alias:      dp.alias,
	// }

	// inner.tables = parenTables{dt}
	// inner.predicates = nil
	// inner.leftJoins = nil
	// return inner
}

func operatorsToRoutes(a, b abstract.PhysicalOperator) (*routeOp, *routeOp) {
	aRoute := makeRouteOp(a)
	if aRoute == nil {
		return nil, nil
	}
	bRoute := makeRouteOp(b)
	if bRoute == nil {
		return nil, nil
	}
	return aRoute, bRoute
}

func tryMergeOp(ctx *context.PlanningContext, a, b abstract.PhysicalOperator, joinPredicates []sqlparser.Expr, merger mergeOpFunc) (abstract.PhysicalOperator, error) {
	aRoute, bRoute := operatorsToRoutes(a.Clone(), b.Clone())
	if aRoute == nil || bRoute == nil {
		return nil, nil
	}

	sameKeyspace := aRoute.keyspace == bRoute.keyspace

	if sameKeyspace || (isDualTableOp(aRoute) || isDualTableOp(bRoute)) {
		tree, err := tryMergeReferenceTableOp(aRoute, bRoute, merger)
		if tree != nil || err != nil {
			return tree, err
		}
	}

	switch aRoute.routeOpCode {
	case engine.SelectUnsharded, engine.SelectDBA:
		if aRoute.routeOpCode == bRoute.routeOpCode {
			return merger(aRoute, bRoute)
		}
	case engine.SelectEqualUnique:
		// if they are already both being sent to the same shard, we can merge
		if bRoute.routeOpCode == engine.SelectEqualUnique {
			if aRoute.selectedVindex() == bRoute.selectedVindex() &&
				gen4ValuesEqual(ctx, aRoute.vindexExpressions(), bRoute.vindexExpressions()) {
				return merger(aRoute, bRoute)
			}
			return nil, nil
		}
		fallthrough
	case engine.SelectScatter, engine.SelectIN:
		if len(joinPredicates) == 0 {
			// If we are doing two Scatters, we have to make sure that the
			// joins are on the correct vindex to allow them to be merged
			// no join predicates - no vindex
			return nil, nil
		}
		if !sameKeyspace {
			return nil, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: cross-shard correlated subquery")
		}

		canMerge := canMergeOpsOnFilters(ctx, aRoute, bRoute, joinPredicates)
		if !canMerge {
			return nil, nil
		}
		r, err := merger(aRoute, bRoute)
		if err != nil {
			return nil, err
		}
		r.pickBestAvailableVindex()
		return r, nil
	}
	return nil, nil
}

func isDualTableOp(route *routeOp) bool {
	sources := leaves(route)
	if len(sources) > 1 {
		return false
	}
	src, ok := sources[0].(*physical.TableOp)
	if !ok {
		return false
	}
	return src.VTable.Name.String() == "dual" && src.QTable.Table.Qualifier.IsEmpty()
}

func leaves(op abstract.Operator) (sources []abstract.Operator) {
	switch op := op.(type) {
	// these are the leaves
	case *abstract.QueryGraph, *abstract.Vindex, *physical.TableOp:
		return []abstract.Operator{op}

		// logical
	case *abstract.Concatenate:
		for _, source := range op.Sources {
			sources = append(sources, leaves(source)...)
		}
		return
	case *abstract.Derived:
		return []abstract.Operator{op.Inner}
	case *abstract.Join:
		return []abstract.Operator{op.LHS, op.RHS}
	case *abstract.SubQuery:
		sources = []abstract.Operator{op.Outer}
		for _, inner := range op.Inner {
			sources = append(sources, inner.Inner)
		}
		return
		// physical
	case *physical.ApplyJoin:
		return []abstract.Operator{op.LHS, op.RHS}
	case *physical.FilterOp:
		return []abstract.Operator{op.Source}
	case *routeOp:
		return []abstract.Operator{op.source}
	}

	panic(fmt.Sprintf("leaves unknown type: %T", op))
}

func tryMergeReferenceTableOp(aRoute, bRoute *routeOp, merger mergeOpFunc) (*routeOp, error) {
	// if either side is a reference table, we can just merge it and use the opcode of the other side
	var opCode engine.RouteOpcode
	var selected *vindexOption

	switch {
	case aRoute.routeOpCode == engine.SelectReference:
		selected = bRoute.selected
		opCode = bRoute.routeOpCode
	case bRoute.routeOpCode == engine.SelectReference:
		selected = aRoute.selected
		opCode = aRoute.routeOpCode
	default:
		return nil, nil
	}

	r, err := merger(aRoute, bRoute)
	if err != nil {
		return nil, err
	}
	r.routeOpCode = opCode
	r.selected = selected
	return r, nil
}

func (r *routeOp) selectedVindex() vindexes.Vindex {
	if r.selected == nil {
		return nil
	}
	return r.selected.foundVindex
}
func (r *routeOp) vindexExpressions() []sqlparser.Expr {
	if r.selected == nil {
		return nil
	}
	return r.selected.valueExprs
}

func canMergeOpsOnFilter(ctx *context.PlanningContext, a, b *routeOp, predicate sqlparser.Expr) bool {
	comparison, ok := predicate.(*sqlparser.ComparisonExpr)
	if !ok {
		return false
	}
	if comparison.Operator != sqlparser.EqualOp {
		return false
	}
	left := comparison.Left
	right := comparison.Right

	lVindex := findColumnVindexOnOps(ctx, a, left)
	if lVindex == nil {
		left, right = right, left
		lVindex = findColumnVindexOnOps(ctx, a, left)
	}
	if lVindex == nil || !lVindex.IsUnique() {
		return false
	}
	rVindex := findColumnVindexOnOps(ctx, b, right)
	if rVindex == nil {
		return false
	}
	return rVindex == lVindex
}

func findColumnVindexOnOps(ctx *context.PlanningContext, a *routeOp, exp sqlparser.Expr) vindexes.SingleColumn {
	_, isCol := exp.(*sqlparser.ColName)
	if !isCol {
		return nil
	}

	var singCol vindexes.SingleColumn

	// for each equality expression that exp has with other column name, we check if it
	// can be solved by any table in our routeTree a. If an equality expression can be solved,
	// we check if the equality expression and our table share the same vindex, if they do:
	// the method will return the associated vindexes.SingleColumn.
	for _, expr := range ctx.SemTable.GetExprAndEqualities(exp) {
		col, isCol := expr.(*sqlparser.ColName)
		if !isCol {
			continue
		}
		leftDep := ctx.SemTable.RecursiveDeps(expr)

		_ = visitOperators(a, func(rel abstract.Operator) (bool, error) {
			to, isTableOp := rel.(*physical.TableOp)
			if !isTableOp {
				return true, nil
			}
			if leftDep.IsSolvedBy(to.QTable.ID) {
				for _, vindex := range to.VTable.ColumnVindexes {
					sC, isSingle := vindex.Vindex.(vindexes.SingleColumn)
					if isSingle && vindex.Columns[0].Equal(col.Name) {
						singCol = sC
						return false, io.EOF
					}
				}
			}
			return false, nil
		})
		if singCol != nil {
			return singCol
		}
	}

	return singCol
}

func canMergeOpsOnFilters(ctx *context.PlanningContext, a, b *routeOp, joinPredicates []sqlparser.Expr) bool {
	for _, predicate := range joinPredicates {
		for _, expr := range sqlparser.SplitAndExpression(nil, predicate) {
			if canMergeOpsOnFilter(ctx, a, b, expr) {
				return true
			}
		}
	}
	return false
}

// visitOperators visits all the operators.
func visitOperators(op abstract.Operator, f func(tbl abstract.Operator) (bool, error)) error {
	kontinue, err := f(op)
	if err != nil {
		return err
	}
	if !kontinue {
		return nil
	}

	switch op := op.(type) {
	case *physical.TableOp, *abstract.QueryGraph, *abstract.Vindex:
		// leaf - no children to visit
	case *routeOp:
		err := visitOperators(op.source, f)
		if err != nil {
			return err
		}
	case *physical.ApplyJoin:
		err := visitOperators(op.LHS, f)
		if err != nil {
			return err
		}
		err = visitOperators(op.RHS, f)
		if err != nil {
			return err
		}
	case *physical.FilterOp:
		err := visitOperators(op.Source, f)
		if err != nil {
			return err
		}
	case *abstract.Concatenate:
		for _, source := range op.Sources {
			err := visitOperators(source, f)
			if err != nil {
				return err
			}
		}
	case *abstract.Derived:
		err := visitOperators(op.Inner, f)
		if err != nil {
			return err
		}
	case *abstract.Join:
		err := visitOperators(op.LHS, f)
		if err != nil {
			return err
		}
		err = visitOperators(op.RHS, f)
		if err != nil {
			return err
		}
	case *abstract.SubQuery:
		err := visitOperators(op.Outer, f)
		if err != nil {
			return err
		}
		for _, source := range op.Inner {
			err := visitOperators(source.Inner, f)
			if err != nil {
				return err
			}
		}
	default:
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "unknown operator type while visiting - %T", op)
	}
	return nil
}

func optimizeUnionOp(ctx *context.PlanningContext, op *abstract.Concatenate) (abstract.PhysicalOperator, error) {
	var sources []abstract.PhysicalOperator

	for _, source := range op.Sources {
		qt, err := createPhysicalOperator(ctx, source)
		if err != nil {
			return nil, err
		}

		sources = append(sources, qt)
	}
	return &physical.UnionOp{Sources: sources, SelectStmts: op.SelectStmts, Distinct: op.Distinct}, nil
}
