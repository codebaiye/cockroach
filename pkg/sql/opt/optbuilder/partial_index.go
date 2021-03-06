// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package optbuilder

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/norm"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// addPartialIndexPredicatesForTable finds all partial indexes in the table and
// adds their predicates to the table metadata (see
// TableMeta.partialIndexPredicates). The predicates are converted from strings
// to ScalarExprs here.
//
// The predicates are used as "known truths" about table data. Any predicates
// containing non-immutable operators are omitted.
//
// scan is an optional argument that is a Scan expression on the table. If scan
// outputs all the ordinary columns in the table, we avoid constructing a new
// scan. A scan and its logical properties are required in order to fully
// normalize the partial index predicates.
func (b *Builder) addPartialIndexPredicatesForTable(tabMeta *opt.TableMeta, scan memo.RelExpr) {
	tab := tabMeta.Table
	numIndexes := tab.DeletableIndexCount()

	// Find the first partial index.
	indexOrd := 0
	for ; indexOrd < numIndexes; indexOrd++ {
		if _, ok := tab.Index(indexOrd).Predicate(); ok {
			break
		}
	}

	// Return early if there are no partial indexes. Only partial indexes have
	// predicates.
	if indexOrd == numIndexes {
		return
	}

	// Construct a scan as the tableScope expr so that logical properties of the
	// scan can be used to fully normalize the index predicate.
	tableScope := b.allocScope()
	tableScope.appendOrdinaryColumnsFromTable(tabMeta, &tabMeta.Alias)

	// If the optional scan argument was provided and it outputs all of the
	// ordinary table columns, we use it as tableScope.expr. Otherwise, we must
	// construct a new scan. Attaching a scan to tableScope.expr is required to
	// fully normalize the partial index predicates with logical properties of
	// the scan.
	if scan != nil && tableScope.colSet().SubsetOf(scan.Relational().OutputCols) {
		tableScope.expr = scan
	} else {
		// TODO(mgartner): This is a sketchy because computed columns won't
		// exist in the scan table's metadata. Currently, the scan argument is
		// nil when building partial index predicates for UPDATEs and UPSERTs
		// table metadata (which are built in order to prune fetch columns). As
		// a result, virtual columns referenced in partial index predicates
		// might not be pruned in all cases that they can be pruned in theory.
		tableScope.expr = b.factory.ConstructScan(&memo.ScanPrivate{
			Table: tabMeta.MetaID,
			Cols:  tableScope.colSet(),
		})
	}

	// Skip to the first partial index we found above.
	for ; indexOrd < numIndexes; indexOrd++ {
		index := tab.Index(indexOrd)
		pred, ok := index.Predicate()

		// If the index is not a partial index, do nothing.
		if !ok {
			continue
		}

		expr, err := parser.ParseExpr(pred)
		if err != nil {
			panic(err)
		}

		// Build the partial index predicate as a memo.FiltersExpr and add it
		// to the table metadata.
		predExpr, err := b.buildPartialIndexPredicate(tabMeta, tableScope, expr, "index predicate")
		if err != nil {
			panic(err)
		}
		tabMeta.AddPartialIndexPredicate(indexOrd, &predExpr)
	}
}

// buildPartialIndexPredicate builds a memo.FiltersExpr from the given
// tree.Expr. Virtual computed columns are inlined as their expressions in the
// resulting filter. Returns an error if any non-immutable operators are found.
//
// Note: This function should only be used to build partial index or arbiter
// predicate expressions that have only a table's ordinary columns in scope and
// that are not part of the relational expression tree. For example, this is
// used to populate the partial index predicates map in TableMeta and for
// determining arbiter indexes in UPSERT and INSERT ON CONFLICT mutations. But
// it is not used for building synthesized mutation columns that determine
// whether to issue PUT or DEL operations on a partial index for a mutated row;
// these synthesized columns are projected as part of the opt expression tree
// and they can reference columns not part of a table's ordinary columns.
func (b *Builder) buildPartialIndexPredicate(
	tabMeta *opt.TableMeta, tableScope *scope, expr tree.Expr, context string,
) (memo.FiltersExpr, error) {
	// A Scan is required as tableScope.expr in order to correctly normalize the
	// partial index predicate. We normalize the predicate by constructing a
	// Select with tableScope.expr as the input and the predicate as filters.
	// This allows normalization rules that only apply to Selects to fire,
	// mimicking query filter normalization. If the input to the Select is not a
	// Scan, additional normalization rules may fire that break assumptions
	// below. For example, if tableScope.expr is a Project expression, the
	// result of constructing and normalizing the Select expression may be a
	// Project expression, making it difficult to access the normalized
	// predicate filters.
	// TODO(mgartner): Consider building a FakeRel specifically for this
	// normalization instead of using tableScope.expr.
	if _, ok := tableScope.expr.(*memo.ScanExpr); !ok {
		panic(errors.AssertionFailedf(
			"can only build partial index predicates with Scan scope expressions, not %T",
			tableScope.expr,
		))
	}

	texpr := resolvePartialIndexPredicate(tableScope, expr)

	var scalar opt.ScalarExpr
	b.factory.FoldingControl().TemporarilyDisallowStableFolds(func() {
		scalar = b.buildScalar(texpr, tableScope, nil, nil, nil)
	})

	// Inline virtual computed column expressions. This is required for
	// partial index predicate implication with virtual columns. A virtual
	// computed column is built as a Project on top of a Scan. The
	// PushSelectIntoInlinableProject normalization rule will push a filter
	// on a virtual computed column below the Project by inlining the
	// virtual column expression. The pushed-down filter will only imply a
	// partial index predicate if the virtual column expression is also
	// inlined in the predicate.
	//
	// Stored computed column expressions do not need to be inlined because
	// they are produced directly from a Scan, not a Project.
	var replace norm.ReplaceFunc
	replace = func(e opt.Expr) opt.Expr {
		switch t := e.(type) {
		case *memo.VariableExpr:
			ord := tabMeta.MetaID.ColumnOrdinal(t.Col)
			col := tabMeta.Table.Column(ord)
			if col.IsVirtualComputed() {
				if expr, ok := tabMeta.ComputedCols[t.Col]; ok {
					return expr
				}
			}
		}
		return b.factory.Replace(e, replace)
	}
	scalar = replace(scalar).(opt.ScalarExpr)

	// Wrap the scalar in a FiltersItem.
	filter := b.factory.ConstructFiltersItem(scalar)

	// Expressions with non-immutable operators are not supported as partial
	// index or arbiter predicates.
	if filter.ScalarProps().VolatilitySet.HasStable() || filter.ScalarProps().VolatilitySet.HasVolatile() {
		return nil, tree.NewContextDependentOpsNotAllowedError(context)
	}

	// Wrap the expression in a FiltersExpr and normalize it by constructing a
	// Select expression.
	filters := memo.FiltersExpr{filter}
	selExpr := b.factory.ConstructSelect(tableScope.expr, filters)

	// If the normalized relational expression is a Select, return the filters.
	if sel, ok := selExpr.(*memo.SelectExpr); ok {
		return sel.Filters, nil
	}

	// Otherwise, the filters may be either true or false. Check the cardinality
	// to determine which one.
	if selExpr.Relational().Cardinality.IsZero() {
		return memo.FiltersExpr{b.factory.ConstructFiltersItem(memo.FalseSingleton)}, nil
	}

	// TODO(mgartner): It is a bit dangerous to assume that if the normalized
	// expression is not a Select and the cardinality is not zero, then the
	// filter is equivalent to True. There should only be 3 types of relation
	// normalized relational expressions: Scan, Values, and Select. Scan
	// indicates the filters are always true and Values indicates the filters
	// are always false. If the expression is a Select, use its filters. If the
	// expression is none of the three, we should probably panic.
	return memo.TrueFilter, nil
}

// resolvePartialIndexPredicate attempts to resolve the type of expr as a
// boolean and return a tree.TypedExpr if successful. It asserts that no errors
// occur during resolution because the predicate should always be valid within
// this context. If an error occurs, it is likely due to a bug in the optimizer.
func resolvePartialIndexPredicate(tableScope *scope, expr tree.Expr) tree.TypedExpr {
	defer func() {
		if r := recover(); r != nil {
			panic(errors.AssertionFailedf("unexpected error during partial index predicate type resolution: %v", r))
		}
	}()
	return tableScope.resolveAndRequireType(expr, types.Bool)
}
