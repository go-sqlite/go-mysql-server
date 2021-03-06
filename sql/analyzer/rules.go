package analyzer

import (
	"strings"

	errors "gopkg.in/src-d/go-errors.v1"
	"gopkg.in/src-d/go-mysql-server.v0/mem"
	"gopkg.in/src-d/go-mysql-server.v0/sql"
	"gopkg.in/src-d/go-mysql-server.v0/sql/expression"
	"gopkg.in/src-d/go-mysql-server.v0/sql/plan"
)

// DefaultRules to apply when analyzing nodes.
var DefaultRules = []Rule{
	{"resolve_subqueries", resolveSubqueries},
	{"resolve_tables", resolveTables},
	{"resolve_orderby_literals", resolveOrderByLiterals},
	{"qualify_columns", qualifyColumns},
	{"resolve_columns", resolveColumns},
	{"resolve_database", resolveDatabase},
	{"resolve_star", resolveStar},
	{"resolve_functions", resolveFunctions},
	{"reorder_projection", reorderProjection},
	{"pushdown", pushdown},
	{"optimize_distinct", optimizeDistinct},
	{"erase_projection", eraseProjection},
	{"index_catalog", indexCatalog},
}

var (
	// ErrColumnTableNotFound is returned when the column does not exist in a
	// the table.
	ErrColumnTableNotFound = errors.NewKind("table %q does not have column %q")
	// ErrColumnNotFound is returned when the column does not exist in any
	// table in scope.
	ErrColumnNotFound = errors.NewKind("column %q could not be found in any table in scope")
	// ErrAmbiguousColumnName is returned when there is a column reference that
	// is present in more than one table.
	ErrAmbiguousColumnName = errors.NewKind("ambiguous column name %q, it's present in all these tables: %v")
	// ErrFieldMissing is returned when the field is not on the schema.
	ErrFieldMissing = errors.NewKind("field %q is not on schema")
	// ErrOrderByColumnIndex is returned when in an order clause there is a
	// column that is unknown.
	ErrOrderByColumnIndex = errors.NewKind("unknown column %d in order by clause")
)

func resolveSubqueries(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("resolve_subqueries")
	defer span.Finish()

	a.Log("resolving subqueries")
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		switch n := n.(type) {
		case *plan.SubqueryAlias:
			a.Log("found subquery %q with child of type %T", n.Name(), n.Child)
			child, err := a.Analyze(ctx, n.Child)
			if err != nil {
				return nil, err
			}
			return plan.NewSubqueryAlias(n.Name(), child), nil
		default:
			return n, nil
		}
	})
}

func resolveOrderByLiterals(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("resolve order by literals")

	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		sort, ok := n.(*plan.Sort)
		if !ok {
			return n, nil
		}

		var fields = make([]plan.SortField, len(sort.SortFields))
		for i, f := range sort.SortFields {
			if lit, ok := f.Column.(*expression.Literal); ok && sql.IsNumber(f.Column.Type()) {
				// it is safe to eval literals with no context and/or row
				v, err := lit.Eval(nil, nil)
				if err != nil {
					return nil, err
				}

				v, err = sql.Int64.Convert(v)
				if err != nil {
					return nil, err
				}

				// column access is 1-indexed
				idx := int(v.(int64)) - 1

				schema := sort.Child.Schema()
				if idx >= len(schema) || idx < 0 {
					return nil, ErrOrderByColumnIndex.New(idx + 1)
				}

				fields[i] = plan.SortField{
					Column:       expression.NewUnresolvedColumn(schema[idx].Name),
					Order:        f.Order,
					NullOrdering: f.NullOrdering,
				}

				a.Log("replaced order by column %d with %s", idx+1, schema[idx].Name)
			} else {
				fields[i] = f
			}
		}

		return plan.NewSort(fields, sort.Child), nil
	})
}

func qualifyColumns(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("qualify_columns")
	defer span.Finish()

	a.Log("qualify columns")
	tables := make(map[string]sql.Node)
	tableAliases := make(map[string]string)
	colIndex := make(map[string][]string)

	indexCols := func(table string, schema sql.Schema) {
		for _, col := range schema {
			colIndex[col.Name] = append(colIndex[col.Name], table)
		}
	}

	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		switch n := n.(type) {
		case *plan.TableAlias:
			switch t := n.Child.(type) {
			case sql.Table:
				tableAliases[n.Name()] = t.Name()
			default:
				tables[n.Name()] = n.Child
				indexCols(n.Name(), n.Schema())
			}
		case sql.Table:
			tables[n.Name()] = n
			indexCols(n.Name(), n.Schema())
		}

		return n.TransformExpressionsUp(func(e sql.Expression) (sql.Expression, error) {
			a.Log("transforming expression of type: %T", e)
			switch col := e.(type) {
			case *expression.UnresolvedColumn:
				col = expression.NewUnresolvedQualifiedColumn(col.Table(), col.Name())

				if col.Table() == "" {
					tables := dedupStrings(colIndex[col.Name()])
					switch len(tables) {
					case 0:
						// If there are no tables that have any column with the column
						// name let's just return it as it is. This may be an alias, so
						// we'll wait for the reorder of the
						return col, nil
					case 1:
						col = expression.NewUnresolvedQualifiedColumn(
							tables[0],
							col.Name(),
						)
					default:
						return nil, ErrAmbiguousColumnName.New(col.Name(), strings.Join(tables, ", "))
					}
				} else {
					if real, ok := tableAliases[col.Table()]; ok {
						col = expression.NewUnresolvedQualifiedColumn(
							real,
							col.Name(),
						)
					}

					if _, ok := tables[col.Table()]; !ok {
						return nil, sql.ErrTableNotFound.New(col.Table())
					}
				}

				a.Log("column %q was qualified with table %q", col.Name(), col.Table())
				return col, nil
			case *expression.Star:
				if col.Table != "" {
					if real, ok := tableAliases[col.Table]; ok {
						col = expression.NewQualifiedStar(real)
					}

					if _, ok := tables[col.Table]; !ok {
						return nil, sql.ErrTableNotFound.New(col.Table)
					}

					return col, nil
				}
			}
			return e, nil
		})
	})
}

func resolveDatabase(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("resolve_database")
	defer span.Finish()

	a.Log("resolve database, node of type: %T", n)

	// TODO Database should implement node,
	// and ShowTables and CreateTable nodes should be binaryNodes
	switch v := n.(type) {
	case *plan.ShowTables:
		db, err := a.Catalog.Database(a.CurrentDatabase)
		if err != nil {
			return n, err
		}

		v.Database = db
	case *plan.CreateTable:
		db, err := a.Catalog.Database(a.CurrentDatabase)
		if err != nil {
			return n, err
		}

		v.Database = db
	}

	return n, nil
}

var dualTable = func() sql.Table {
	t := mem.NewTable("dual", sql.Schema{
		{Name: "dummy", Source: "dual", Type: sql.Text, Nullable: false},
	})
	_ = t.Insert(sql.NewRow("x"))
	return t
}()

func resolveTables(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("resolve_tables")
	defer span.Finish()

	a.Log("resolve table, node of type: %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		t, ok := n.(*plan.UnresolvedTable)
		if !ok {
			return n, nil
		}

		rt, err := a.Catalog.Table(a.CurrentDatabase, t.Name)
		if err != nil {
			if sql.ErrTableNotFound.Is(err) && t.Name == dualTable.Name() {
				rt = dualTable
			} else {
				return nil, err
			}
		}

		a.Log("table resolved: %q", rt.Name())

		return rt, nil
	})
}

func resolveStar(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("resolve_star")
	defer span.Finish()

	a.Log("resolving star, node of type: %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		p, ok := n.(*plan.Project)
		if !ok {
			return n, nil
		}

		var expressions []sql.Expression
		schema := p.Child.Schema()
		for _, e := range p.Projections {
			if s, ok := e.(*expression.Star); ok {
				var exprs []sql.Expression
				for i, col := range schema {
					if s.Table == "" || s.Table == col.Source {
						exprs = append(exprs, expression.NewGetFieldWithTable(
							i, col.Type, col.Source, col.Name, col.Nullable,
						))
					}
				}

				if len(exprs) == 0 && s.Table != "" {
					return nil, sql.ErrTableNotFound.New(s.Table)
				}

				a.Log("%s replaced with %d fields", e, len(exprs))
				expressions = append(expressions, exprs...)
			} else {
				expressions = append(expressions, e)
			}
		}

		return plan.NewProject(expressions, p.Child), nil
	})
}

type columnInfo struct {
	idx int
	col *sql.Column
}

// maybeAlias is a wrapper on UnresolvedColumn used only to defer the
// resolution of the column because it could be an alias and that
// phase of the analyzer has not run yet.
type maybeAlias struct {
	*expression.UnresolvedColumn
}

func (e maybeAlias) TransformUp(fn sql.TransformExprFunc) (sql.Expression, error) {
	return fn(e)
}

// column is the common interface that groups UnresolvedColumn and maybeAlias.
type column interface {
	sql.Nameable
	sql.Tableable
	sql.Expression
}

func resolveColumns(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("resolve_columns")
	defer span.Finish()

	a.Log("resolve columns, node of type: %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		colMap := make(map[string][]columnInfo)
		idx := 0
		for _, child := range n.Children() {
			if !child.Resolved() {
				return n, nil
			}

			for _, col := range child.Schema() {
				colMap[col.Name] = append(colMap[col.Name], columnInfo{idx, col})
				idx++
			}
		}

		return n.TransformExpressionsUp(func(e sql.Expression) (sql.Expression, error) {
			a.Log("transforming expression of type: %T", e)
			if n.Resolved() {
				return e, nil
			}

			uc, ok := e.(column)
			if !ok {
				return e, nil
			}

			columnsInfo, ok := colMap[uc.Name()]
			if !ok {
				if uc.Table() != "" {
					return nil, ErrColumnTableNotFound.New(uc.Table(), uc.Name())
				}

				switch uc := uc.(type) {
				case *expression.UnresolvedColumn:
					return &maybeAlias{uc}, nil
				default:
					return nil, ErrColumnNotFound.New(uc.Name())
				}
			}

			var ci columnInfo
			var found bool
			for _, c := range columnsInfo {
				if c.col.Source == uc.Table() {
					ci = c
					found = true
					break
				}
			}

			if !found {
				if uc.Table() != "" {
					return nil, ErrColumnTableNotFound.New(uc.Table(), uc.Name())
				}

				switch uc := uc.(type) {
				case *expression.UnresolvedColumn:
					return &maybeAlias{uc}, nil
				default:
					return nil, ErrColumnNotFound.New(uc.Name())
				}
			}

			a.Log("column resolved to %q.%q", ci.col.Source, ci.col.Name)

			return expression.NewGetFieldWithTable(
				ci.idx,
				ci.col.Type,
				ci.col.Source,
				ci.col.Name,
				ci.col.Nullable,
			), nil
		})
	})
}

func resolveFunctions(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("resolve_functions")
	defer span.Finish()

	a.Log("resolve functions, node of type %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		return n.TransformExpressionsUp(func(e sql.Expression) (sql.Expression, error) {
			a.Log("transforming expression of type: %T", e)
			if e.Resolved() {
				return e, nil
			}

			uf, ok := e.(*expression.UnresolvedFunction)
			if !ok {
				return e, nil
			}

			n := uf.Name()
			f, err := a.Catalog.Function(n)
			if err != nil {
				return nil, err
			}

			rf, err := f.Call(uf.Arguments...)
			if err != nil {
				return nil, err
			}

			a.Log("resolved function %q", n)

			return rf, nil
		})
	})
}

func optimizeDistinct(ctx *sql.Context, a *Analyzer, node sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("optimize_distinct")
	defer span.Finish()

	a.Log("optimize distinct, node of type: %T", node)
	if node, ok := node.(*plan.Distinct); ok {
		var isSorted bool
		_, _ = node.TransformUp(func(node sql.Node) (sql.Node, error) {
			a.Log("checking for optimization in node of type: %T", node)
			if _, ok := node.(*plan.Sort); ok {
				isSorted = true
			}
			return node, nil
		})

		if isSorted {
			a.Log("distinct optimized for ordered output")
			return plan.NewOrderedDistinct(node.Child), nil
		}
	}

	return node, nil
}

var errInvalidNodeType = errors.NewKind("reorder projection: invalid node of type: %T")

func reorderProjection(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("reorder_projection")
	defer span.Finish()

	if n.Resolved() {
		return n, nil
	}

	a.Log("reorder projection, node of type: %T", n)

	// Then we transform the projection
	return n.TransformUp(func(node sql.Node) (sql.Node, error) {
		project, ok := node.(*plan.Project)
		if !ok {
			return node, nil
		}

		// We must find all columns that may need to be moved inside the
		// projection.
		//var movedColumns = make(map[string]sql.Expression)
		var newColumns = make(map[string]sql.Expression)
		for _, col := range project.Projections {
			alias, ok := col.(*expression.Alias)
			if ok {
				newColumns[alias.Name()] = col
			}
		}

		// And add projection nodes where needed in the child tree.
		var didNeedReorder bool
		child, err := project.Child.TransformUp(func(node sql.Node) (sql.Node, error) {
			var requiredColumns []string
			switch node := node.(type) {
			case *plan.Sort, *plan.Filter:
				for _, expr := range node.(sql.Expressioner).Expressions() {
					expression.Inspect(expr, func(e sql.Expression) bool {
						if e != nil && e.Resolved() {
							return true
						}

						uc, ok := e.(column)
						if ok && uc.Table() == "" {
							if _, ok := newColumns[uc.Name()]; ok {
								requiredColumns = append(requiredColumns, uc.Name())
							}
						}

						return true
					})
				}
			default:
				return node, nil
			}

			didNeedReorder = true

			// Only add the required columns for that node in the projection.
			child := node.Children()[0]
			schema := child.Schema()
			var projections = make([]sql.Expression, 0, len(schema)+len(requiredColumns))
			for i, col := range schema {
				projections = append(projections, expression.NewGetFieldWithTable(
					i, col.Type, col.Source, col.Name, col.Nullable,
				))
			}

			for _, col := range requiredColumns {
				projections = append(projections, newColumns[col])
				delete(newColumns, col)
			}

			child = plan.NewProject(projections, child)
			switch node := node.(type) {
			case *plan.Filter:
				return plan.NewFilter(node.Expression, child), nil
			case *plan.Sort:
				return plan.NewSort(node.SortFields, child), nil
			default:
				return nil, errInvalidNodeType.New(node)
			}
		})

		if err != nil {
			return nil, err
		}

		if !didNeedReorder {
			return project, nil
		}

		child, err = resolveColumns(ctx, a, child)
		if err != nil {
			return nil, err
		}

		childSchema := child.Schema()
		// Finally, replace the columns we moved with GetFields since they
		// have already been projected.
		var projections = make([]sql.Expression, len(project.Projections))
		for i, p := range project.Projections {
			if alias, ok := p.(*expression.Alias); ok {
				var found bool
				for idx, col := range childSchema {
					if col.Name == alias.Name() {
						projections[i] = expression.NewGetField(
							idx, col.Type, col.Name, col.Nullable,
						)
						found = true
						break
					}
				}

				if !found {
					projections[i] = p
				}
			} else {
				projections[i] = p
			}
		}

		return plan.NewProject(projections, child), nil
	})
}

func eraseProjection(ctx *sql.Context, a *Analyzer, node sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("erase_projection")
	defer span.Finish()

	if !node.Resolved() {
		return node, nil
	}

	a.Log("erase projection, node of type: %T", node)

	return node.TransformUp(func(node sql.Node) (sql.Node, error) {
		project, ok := node.(*plan.Project)
		if ok && project.Schema().Equals(project.Child.Schema()) {
			a.Log("project erased")
			return project.Child, nil
		}

		return node, nil
	})
}

func dedupStrings(in []string) []string {
	var seen = make(map[string]struct{})
	var result []string
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			result = append(result, s)
		}
	}
	return result
}

// indexCatalog sets the catalog in the CreateIndex nodes.
func indexCatalog(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	if !n.Resolved() {
		return n, nil
	}

	ci, ok := n.(*plan.CreateIndex)
	if !ok {
		return n, nil
	}

	span, ctx := ctx.Span("index_catalog")
	defer span.Finish()

	nc := *ci
	ci.Catalog = a.Catalog
	ci.CurrentDatabase = a.CurrentDatabase

	return &nc, nil
}

func pushdown(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("pushdown")
	defer span.Finish()

	a.Log("pushdown, node of type: %T", n)
	if !n.Resolved() {
		return n, nil
	}

	var fieldsByTable = make(map[string][]string)
	var exprsByTable = make(map[string][]sql.Expression)
	type tableField struct {
		table string
		field string
	}
	var tableFields = make(map[tableField]struct{})

	a.Log("finding used columns in node")

	colSpan, _ := ctx.Span("find_pushdown_columns")

	// First step is to find all col exprs and group them by the table they mention.
	// Even if they appear multiple times, only the first one will be used.
	plan.InspectExpressions(n, func(e sql.Expression) bool {
		if e, ok := e.(*expression.GetField); ok {
			tf := tableField{e.Table(), e.Name()}
			if _, ok := tableFields[tf]; !ok {
				a.Log("found used column %s.%s", e.Table(), e.Name())
				tableFields[tf] = struct{}{}
				fieldsByTable[e.Table()] = append(fieldsByTable[e.Table()], e.Name())
				exprsByTable[e.Table()] = append(exprsByTable[e.Table()], e)
			}
		}
		return true
	})

	colSpan.Finish()

	a.Log("finding filters in node")

	filterSpan, _ := ctx.Span("find_pushdown_filters")

	// then find all filters, also by table. Note that filters that mention
	// more than one table will not be passed to neither.
	filters := make(filters)
	plan.Inspect(n, func(node sql.Node) bool {
		a.Log("inspecting node of type: %T", node)
		switch node := node.(type) {
		case *plan.Filter:
			fs := exprToTableFilters(node.Expression)
			a.Log("found filters for %d tables %s", len(fs), node.Expression)
			filters.merge(fs)
		}
		return true
	})

	filterSpan.Finish()

	a.Log("transforming nodes with pushdown of filters and projections")

	// Now all nodes can be transformed. Since traversal of the tree is done
	// from inner to outer the filters have to be processed first so they get
	// to the tables.
	var handledFilters []sql.Expression
	return n.TransformUp(func(node sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", node)
		switch node := node.(type) {
		case *plan.Filter:
			if len(handledFilters) == 0 {
				a.Log("no handled filters, leaving filter untouched")
				return node, nil
			}

			unhandled := getUnhandledFilters(
				splitExpression(node.Expression),
				handledFilters,
			)

			if len(unhandled) == 0 {
				a.Log("filter node has no unhandled filters, so it will be removed")
				return node.Child, nil
			}

			a.Log(
				"%d handled filters removed from filter node, filter has now %d filters",
				len(handledFilters),
				len(unhandled),
			)

			return plan.NewFilter(expression.JoinAnd(unhandled...), node.Child), nil
		case *plan.PushdownProjectionAndFiltersTable, *plan.PushdownProjectionTable:
			// they also implement the interfaces for pushdown, so we better return
			// or there will be a very nice infinite loop
			return node, nil
		case sql.PushdownProjectionAndFiltersTable:
			cols := exprsByTable[node.Name()]
			tableFilters := filters[node.Name()]
			handled := node.HandledFilters(tableFilters)
			handledFilters = append(handledFilters, handled...)

			a.Log(
				"table %q transformed with pushdown of projection and filters, %d filters handled of %d",
				node.Name(),
				len(handled),
				len(tableFilters),
			)

			schema := node.Schema()
			cols, err := fixFieldIndexesOnExpressions(schema, cols...)
			if err != nil {
				return nil, err
			}

			handled, err = fixFieldIndexesOnExpressions(schema, handled...)
			if err != nil {
				return nil, err
			}

			return plan.NewPushdownProjectionAndFiltersTable(
				cols,
				handled,
				node,
			), nil
		case sql.PushdownProjectionTable:
			cols := fieldsByTable[node.Name()]
			a.Log("table %q transformed with pushdown of projection", node.Name())
			return plan.NewPushdownProjectionTable(cols, node), nil
		}
		return node, nil
	})
}

// fixFieldIndexesOnExpressions executes fixFieldIndexes on a list of exprs.
func fixFieldIndexesOnExpressions(schema sql.Schema, expressions ...sql.Expression) ([]sql.Expression, error) {
	var result = make([]sql.Expression, len(expressions))
	for i, e := range expressions {
		var err error
		result[i], err = fixFieldIndexes(schema, e)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// fixFieldIndexes transforms the given expression setting correct indexes
// for GetField expressions according to the schema of the row in the table
// and not the one where the filter came from.
func fixFieldIndexes(schema sql.Schema, exp sql.Expression) (sql.Expression, error) {
	return exp.TransformUp(func(e sql.Expression) (sql.Expression, error) {
		switch e := e.(type) {
		case *expression.GetField:
			// we need to rewrite the indexes for the table row
			for i, col := range schema {
				if e.Name() == col.Name {
					return expression.NewGetFieldWithTable(
						i,
						e.Type(),
						e.Table(),
						e.Name(),
						e.IsNullable(),
					), nil
				}
			}

			return nil, ErrFieldMissing.New(e.Name())
		}

		return e, nil
	})
}
