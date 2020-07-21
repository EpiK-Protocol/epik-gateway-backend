package linkedql

import (
	"context"

	"github.com/cayleygraph/quad"
	"github.com/cayleygraph/quad/jsonld"
	"github.com/cayleygraph/quad/voc"
	"github.com/epik-protocol/gateway/graph"
	"github.com/epik-protocol/gateway/graph/iterator"
	"github.com/epik-protocol/gateway/graph/refs"
	"github.com/epik-protocol/gateway/query"
	"github.com/epik-protocol/gateway/query/path"
)

var _ query.Iterator = (*ValueIterator)(nil)

// ValueIterator is an iterator of values from the graph.
type ValueIterator struct {
	Namer   refs.Namer
	path    *path.Path
	scanner iterator.Scanner
}

// NewValueIterator returns a new ValueIterator for a path and namer.
func NewValueIterator(p *path.Path, namer refs.Namer) *ValueIterator {
	return &ValueIterator{Namer: namer, path: p}
}

// NewValueIteratorFromPathStep attempts to build a path from PathStep and return a new ValueIterator of it.
// If BuildPath fails returns error.
func NewValueIteratorFromPathStep(step PathStep, qs graph.QuadStore, ns *voc.Namespaces) (*ValueIterator, error) {
	p, err := step.BuildPath(qs, ns)
	if err != nil {
		return nil, err
	}
	return NewValueIterator(p, qs), nil
}

// Next implements query.Iterator.
func (it *ValueIterator) Next(ctx context.Context) bool {
	if it.scanner == nil {
		it.scanner = it.path.BuildIterator(ctx).Iterate()
	}
	return it.scanner.Next(ctx)
}

// Value returns the current value
func (it *ValueIterator) Value() quad.Value {
	if it.scanner == nil {
		return nil
	}
	return it.Namer.NameOf(it.scanner.Result())
}

// Result implements query.Iterator.
func (it *ValueIterator) Result() interface{} {
	// FIXME(iddan): only convert when collation is JSON/JSON-LD, leave as Ref otherwise
	return jsonld.FromValue(it.Value())
}

// Err implements query.Iterator.
func (it *ValueIterator) Err() error {
	if it.scanner == nil {
		return nil
	}
	return it.scanner.Err()
}

// Close implements query.Iterator.
func (it *ValueIterator) Close() error {
	if it.scanner == nil {
		return nil
	}
	return it.scanner.Close()
}
