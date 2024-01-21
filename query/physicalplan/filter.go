package physicalplan

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/RoaringBitmap/roaring"
	"github.com/apache/arrow/go/v14/arrow"
	"github.com/apache/arrow/go/v14/arrow/array"
	"github.com/apache/arrow/go/v14/arrow/compute"
	"github.com/apache/arrow/go/v14/arrow/memory"
	"github.com/apache/arrow/go/v14/arrow/scalar"
	"go.opentelemetry.io/otel/trace"

	"github.com/polarsignals/frostdb/pqarrow/arrowutils"
	"github.com/polarsignals/frostdb/query/logicalplan"
)

type PredicateFilter struct {
	pool       memory.Allocator
	tracer     trace.Tracer
	filterExpr BooleanExpression
	next       PhysicalPlan
}

func (f *PredicateFilter) Draw() *Diagram {
	var child *Diagram
	if f.next != nil {
		child = f.next.Draw()
	}
	details := fmt.Sprintf("PredicateFilter (%s)", f.filterExpr.String())
	return &Diagram{Details: details, Child: child}
}

type Bitmap = roaring.Bitmap

func NewBitmap() *Bitmap {
	return roaring.New()
}

type BooleanExpression interface {
	Eval(r arrow.Record) (*Bitmap, error)
	String() string
}

var ErrUnsupportedBooleanExpression = errors.New("unsupported boolean expression")

type ArrayReference struct{}

type PreExprVisitorFunc func(expr logicalplan.Expr) bool

func (f PreExprVisitorFunc) PreVisit(expr logicalplan.Expr) bool {
	return f(expr)
}

func (f PreExprVisitorFunc) Visit(_ logicalplan.Expr) bool {
	return false
}

func (f PreExprVisitorFunc) PostVisit(_ logicalplan.Expr) bool {
	return false
}

func binaryBooleanExpr(expr *logicalplan.BinaryExpr) (BooleanExpression, error) {
	switch expr.Op {
	case logicalplan.OpEq, logicalplan.OpNotEq, logicalplan.OpLt, logicalplan.OpLtEq, logicalplan.OpGt, logicalplan.OpGtEq, logicalplan.OpRegexMatch, logicalplan.OpRegexNotMatch:
		var leftColumnRef *ArrayRef
		expr.Left.Accept(PreExprVisitorFunc(func(expr logicalplan.Expr) bool {
			switch e := expr.(type) {
			case *logicalplan.Column:
				leftColumnRef = &ArrayRef{
					ColumnName: e.ColumnName,
				}
				return false
			}
			return true
		}))
		if leftColumnRef == nil {
			return nil, errors.New("left side of binary expression must be a column")
		}

		var rightScalar scalar.Scalar
		expr.Right.Accept(PreExprVisitorFunc(func(expr logicalplan.Expr) bool {
			switch e := expr.(type) {
			case *logicalplan.LiteralExpr:
				rightScalar = e.Value
				return false
			}
			return true
		}))

		switch expr.Op {
		case logicalplan.OpRegexMatch:
			regexp, err := regexp.Compile(string(rightScalar.(*scalar.String).Data()))
			if err != nil {
				return nil, err
			}
			return &RegExpFilter{
				left:  leftColumnRef,
				right: regexp,
			}, nil
		case logicalplan.OpRegexNotMatch:
			regexp, err := regexp.Compile(string(rightScalar.(*scalar.String).Data()))
			if err != nil {
				return nil, err
			}
			return &RegExpFilter{
				left:     leftColumnRef,
				right:    regexp,
				notMatch: true,
			}, nil
		}

		return &BinaryScalarExpr{
			Left:  leftColumnRef,
			Op:    expr.Op,
			Right: rightScalar,
		}, nil
	case logicalplan.OpAnd:
		left, err := booleanExpr(expr.Left)
		if err != nil {
			return nil, err
		}

		right, err := booleanExpr(expr.Right)
		if err != nil {
			return nil, err
		}

		return &AndExpr{
			Left:  left,
			Right: right,
		}, nil
	case logicalplan.OpOr:
		left, err := booleanExpr(expr.Left)
		if err != nil {
			return nil, err
		}

		right, err := booleanExpr(expr.Right)
		if err != nil {
			return nil, err
		}

		return &OrExpr{
			Left:  left,
			Right: right,
		}, nil
	default:
		panic("unsupported binary boolean expression")
	}
}

type AndExpr struct {
	Left  BooleanExpression
	Right BooleanExpression
}

func (a *AndExpr) Eval(r arrow.Record) (*Bitmap, error) {
	left, err := a.Left.Eval(r)
	if err != nil {
		return nil, err
	}

	right, err := a.Right.Eval(r)
	if err != nil {
		return nil, err
	}

	// This stores the result in place to avoid allocations.
	left.And(right)
	return left, nil
}

func (a *AndExpr) String() string {
	return "(" + a.Left.String() + " AND " + a.Right.String() + ")"
}

type OrExpr struct {
	Left  BooleanExpression
	Right BooleanExpression
}

func (a *OrExpr) Eval(r arrow.Record) (*Bitmap, error) {
	left, err := a.Left.Eval(r)
	if err != nil {
		return nil, err
	}

	right, err := a.Right.Eval(r)
	if err != nil {
		return nil, err
	}

	// This stores the result in place to avoid allocations.
	left.Or(right)
	return left, nil
}

func (a *OrExpr) String() string {
	return "(" + a.Left.String() + " OR " + a.Right.String() + ")"
}

func booleanExpr(expr logicalplan.Expr) (BooleanExpression, error) {
	switch e := expr.(type) {
	case *logicalplan.BinaryExpr:
		return binaryBooleanExpr(e)
	default:
		return nil, ErrUnsupportedBooleanExpression
	}
}

func Filter(pool memory.Allocator, tracer trace.Tracer, filterExpr logicalplan.Expr) (*PredicateFilter, error) {
	expr, err := booleanExpr(filterExpr)
	if err != nil {
		return nil, err
	}

	return newFilter(pool, tracer, expr), nil
}

func newFilter(pool memory.Allocator, tracer trace.Tracer, filterExpr BooleanExpression) *PredicateFilter {
	return &PredicateFilter{
		pool:       pool,
		tracer:     tracer,
		filterExpr: filterExpr,
	}
}

func (f *PredicateFilter) SetNext(next PhysicalPlan) {
	f.next = next
}

func (f *PredicateFilter) Close() {
	f.next.Close()
}

func (f *PredicateFilter) Callback(ctx context.Context, r arrow.Record) error {
	// Generates high volume of spans. Comment out if needed during development.
	// ctx, span := f.tracer.Start(ctx, "PredicateFilter/Callback")
	// defer span.End()

	filtered, empty, err := filter(f.pool, f.filterExpr, r)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}

	defer filtered.Release()
	return f.next.Callback(ctx, filtered)
}

func (f *PredicateFilter) Finish(ctx context.Context) error {
	return f.next.Finish(ctx)
}

func filter(pool memory.Allocator, filterExpr BooleanExpression, ar arrow.Record) (arrow.Record, bool, error) {
	bitmap, err := filterExpr.Eval(ar)
	if err != nil {
		return nil, true, err
	}

	if bitmap.IsEmpty() {
		return nil, true, nil
	}
	b := array.NewInt32Builder(pool)
	defer b.Release()
	b.Reserve(int(bitmap.GetCardinality()))
	bitmap.Iterate(func(x uint32) bool {
		b.UnsafeAppend(int32(x))
		return true
	})
	indices := b.NewInt32Array()
	defer indices.Release()
	r, err := arrowutils.Take(
		compute.WithAllocator(context.Background(), pool), ar, indices,
	)
	if err != nil {
		return nil, true, err
	}
	return r, false, nil
}

type IndexRange struct {
	Start uint32
	End   uint32
}

// buildIndexRanges returns a set of continguous index ranges from the given indicies
// ex: [1,2,7,8,9] would return [{Start:1, End:3},{Start:7,End:10}]
func buildIndexRanges(indices []uint32) []IndexRange {
	ranges := []IndexRange{}

	cur := IndexRange{
		Start: indices[0],
		End:   indices[0] + 1,
	}

	for _, i := range indices[1:] {
		if i == cur.End {
			cur.End++
		} else {
			ranges = append(ranges, cur)
			cur = IndexRange{
				Start: i,
				End:   i + 1,
			}
		}
	}

	ranges = append(ranges, cur)
	return ranges
}
