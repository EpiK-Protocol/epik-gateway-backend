// Copyright 2014 The Cayley Authors. All rights reserved.
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

package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cayleygraph/quad"
	"github.com/epik-protocol/epik-gateway-backend/clog"
	"github.com/epik-protocol/epik-gateway-backend/graph"
	"github.com/epik-protocol/epik-gateway-backend/graph/iterator"
	"github.com/epik-protocol/epik-gateway-backend/graph/refs"
)

const QuadStoreType = "memstore"

var EmptyQuad quad.Quad

func init() {
	graph.RegisterQuadStore(QuadStoreType, graph.QuadStoreRegistration{
		NewFunc: func(string, graph.Options) (graph.QuadStore, error) {
			return newQuadStore(), nil
		},
		UpgradeFunc:  nil,
		InitFunc:     nil,
		IsPersistent: false,
	})
}

type bnode int64

func (n bnode) Key() interface{} { return n }

type qprim struct {
	p *Primitive
}

func (n qprim) Key() interface{} { return n.p.ID }

var _ quad.Writer = (*QuadStore)(nil)

func cmp(a, b int64) int {
	return int(a - b)
}

type QuadDirectionIndex struct {
	index [4]map[int64]*Tree
}

func NewQuadDirectionIndex() QuadDirectionIndex {
	return QuadDirectionIndex{[...]map[int64]*Tree{
		quad.Subject - 1:   make(map[int64]*Tree),
		quad.Predicate - 1: make(map[int64]*Tree),
		quad.Object - 1:    make(map[int64]*Tree),
		quad.Label - 1:     make(map[int64]*Tree),
	}}
}

func (qdi QuadDirectionIndex) Tree(d quad.Direction, id int64) *Tree {
	if d < quad.Subject || d > quad.Label {
		panic("illegal direction")
	}
	tree, ok := qdi.index[d-1][id]
	if !ok {
		tree = TreeNew(cmp)
		qdi.index[d-1][id] = tree
	}
	return tree
}

func (qdi QuadDirectionIndex) Get(d quad.Direction, id int64) (*Tree, bool) {
	if d < quad.Subject || d > quad.Label {
		panic("illegal direction")
	}
	tree, ok := qdi.index[d-1][id]
	return tree, ok
}

type CidQuadIndex struct {
	index map[string]int64
}

func NewCidQuadIndex() CidQuadIndex {
	return CidQuadIndex{map[string]int64{}}
}

func (cqi CidQuadIndex) Add(cid string, id int64) bool {
	if len(cid) == 0 || id == 0 {
		return false
	}
	_, exist := cqi.index[cid]
	if !exist {
		cqi.index[cid] = id
	}
	return !exist
}

func (cqi CidQuadIndex) Delete(cid string) bool {
	_, exist := cqi.index[cid]
	if exist {
		delete(cqi.index, cid)
	}
	return exist
}

func (cqi CidQuadIndex) Get(cid string) (int64, bool) {
	qid, exist := cqi.index[cid]
	return qid, exist
}

type Primitive struct {
	ID    int64
	Quad  internalQuad
	Value quad.Value
	refs  int
}

type internalQuad struct {
	S, P, O, L int64
}

func (q internalQuad) Zero() bool {
	return q == (internalQuad{})
}

func (q *internalQuad) SetDir(d quad.Direction, n int64) {
	switch d {
	case quad.Subject:
		q.S = n
	case quad.Predicate:
		q.P = n
	case quad.Object:
		q.O = n
	case quad.Label:
		q.L = n
	default:
		panic(fmt.Errorf("unknown dir: %v", d))
	}
}
func (q internalQuad) Dir(d quad.Direction) int64 {
	var n int64
	switch d {
	case quad.Subject:
		n = q.S
	case quad.Predicate:
		n = q.P
	case quad.Object:
		n = q.O
	case quad.Label:
		n = q.L
	}
	return n
}

type QuadStore struct {
	last int64
	// TODO: string -> quad.Value once Raw -> typed resolution is unnecessary
	vals    map[string]int64
	quads   map[internalQuad]int64
	prim    map[int64]*Primitive
	all     []*Primitive // might not be sorted by id
	reading bool         // someone else might be reading "all" slice - next insert/delete should clone it
	index   QuadDirectionIndex
	horizon int64 // used only to assign ids to tx
	// vip_index map[string]map[int64]map[string]map[int64]*b.Tree

	epoch   int64
	cqIndex CidQuadIndex
}

// New creates a new in-memory quad store and loads provided quads.
func New(quads ...quad.Quad) *QuadStore {
	qs := newQuadStore()
	for _, q := range quads {
		qs.AddQuad(q)
	}
	return qs
}

func newQuadStore() *QuadStore {
	return &QuadStore{
		vals:    make(map[string]int64),
		quads:   make(map[internalQuad]int64),
		prim:    make(map[int64]*Primitive),
		index:   NewQuadDirectionIndex(),
		cqIndex: NewCidQuadIndex(),
	}
}

func (qs *QuadStore) cloneAll() []*Primitive {
	qs.reading = true
	return qs.all
}

func (qs *QuadStore) addPrimitive(p *Primitive) int64 {
	qs.last++
	id := qs.last
	p.ID = id
	p.refs = 1
	qs.appendPrimitive(p)
	return id
}

func (qs *QuadStore) appendPrimitive(p *Primitive) {
	qs.prim[p.ID] = p
	if !qs.reading {
		qs.all = append(qs.all, p)
	} else {
		n := len(qs.all)
		qs.all = append(qs.all[:n:n], p) // reallocate slice
		qs.reading = false               // this is a new slice
	}
}

const internalBNodePrefix = "memnode"

func (qs *QuadStore) resolveVal(v quad.Value, add bool) (int64, bool) {
	if v == nil {
		return 0, false
	}
	if n, ok := v.(quad.BNode); ok && strings.HasPrefix(string(n), internalBNodePrefix) {
		n = n[len(internalBNodePrefix):]
		id, err := strconv.ParseInt(string(n), 10, 64)
		if err == nil && id != 0 {
			if p, ok := qs.prim[id]; ok || !add {
				if add {
					p.refs++
				}
				return id, ok
			}
			qs.appendPrimitive(&Primitive{ID: id, refs: 1})
			return id, true
		}
	}
	vs := v.String()
	if id, exists := qs.vals[vs]; exists || !add {
		if exists && add {
			qs.prim[id].refs++
		}
		return id, exists
	}
	id := qs.addPrimitive(&Primitive{Value: v})
	qs.vals[vs] = id
	return id, true
}

func (qs *QuadStore) resolveQuad(q quad.Quad, add bool) (internalQuad, bool) {
	var p internalQuad
	for dir := quad.Subject; dir <= quad.Label; dir++ {
		v := q.Get(dir)
		if v == nil {
			continue
		}
		if vid, _ := qs.resolveVal(v, add); vid != 0 {
			p.SetDir(dir, vid)
		} else if !add {
			return internalQuad{}, false
		}
	}
	return p, true
}

func (qs *QuadStore) lookupVal(id int64) quad.Value {
	pv := qs.prim[id]
	if pv == nil || pv.Value == nil {
		return quad.BNode(internalBNodePrefix + strconv.FormatInt(id, 10))
	}
	return pv.Value
}

func (qs *QuadStore) lookupQuadDirs(p internalQuad) quad.Quad {
	var q quad.Quad
	for dir := quad.Subject; dir <= quad.Label; dir++ {
		vid := p.Dir(dir)
		if vid == 0 {
			continue
		}
		v := qs.lookupVal(vid)
		q.Set(dir, v)
	}
	return q
}

// AddNode adds a blank node (with no value) to quad store. It returns an id of the node.
func (qs *QuadStore) AddBNode() int64 {
	return qs.addPrimitive(&Primitive{})
}

// AddNode adds a value to quad store. It returns an id of the value.
// False is returned as a second parameter if value exists already.
func (qs *QuadStore) AddValue(v quad.Value) (int64, bool) {
	id, exists := qs.resolveVal(v, true)
	return id, !exists
}

func (qs *QuadStore) indexesForQuad(q internalQuad) []*Tree {
	trees := make([]*Tree, 0, 4)
	for dir := quad.Subject; dir <= quad.Label; dir++ {
		v := q.Dir(dir)
		if v == 0 {
			continue
		}
		trees = append(trees, qs.index.Tree(dir, v))
	}
	return trees
}

// AddQuad adds a quad to quad store. It returns an id of the quad.
// False is returned as a second parameter if quad exists already.
func (qs *QuadStore) AddQuad(q quad.Quad) (int64, bool) {
	p, _ := qs.resolveQuad(q, false)
	if id := qs.quads[p]; id != 0 {
		return id, false
	}
	p, _ = qs.resolveQuad(q, true)
	pr := &Primitive{Quad: p}
	id := qs.addPrimitive(pr)
	qs.quads[p] = id
	for _, t := range qs.indexesForQuad(p) {
		t.Set(id, pr)
	}
	// TODO(barakmich): Add VIP indexing
	return id, true
}

// WriteQuad adds a quad to quad store.
//
// Deprecated: use AddQuad instead.
func (qs *QuadStore) WriteQuad(q quad.Quad) error {
	qs.AddQuad(q)
	return nil
}

// WriteQuads implements quad.Writer.
func (qs *QuadStore) WriteQuads(buf []quad.Quad) (int, error) {
	for _, q := range buf {
		qs.AddQuad(q)
	}
	return len(buf), nil
}

func (qs *QuadStore) NewQuadWriter() (quad.WriteCloser, error) {
	return &quadWriter{qs: qs}, nil
}

type quadWriter struct {
	qs *QuadStore
}

func (w *quadWriter) WriteQuad(q quad.Quad) error {
	w.qs.AddQuad(q)
	return nil
}

func (w *quadWriter) WriteQuads(buf []quad.Quad) (int, error) {
	for _, q := range buf {
		w.qs.AddQuad(q)
	}
	return len(buf), nil
}

func (w *quadWriter) Close() error {
	return nil
}

func (qs *QuadStore) deleteQuadNodes(q internalQuad) {
	for dir := quad.Subject; dir <= quad.Label; dir++ {
		id := q.Dir(dir)
		if id == 0 {
			continue
		}
		if p := qs.prim[id]; p != nil {
			p.refs--
			if p.refs < 0 {
				panic("remove of deleted node")
			} else if p.refs == 0 {
				qs.Delete(id)
			}
		}
	}
}
func (qs *QuadStore) Delete(id int64) bool {
	p := qs.prim[id]
	if p == nil {
		return false
	}
	// remove from value index
	if p.Value != nil {
		delete(qs.vals, p.Value.String())
	}
	// remove from quad indexes
	for _, t := range qs.indexesForQuad(p.Quad) {
		t.Delete(id)
	}
	delete(qs.quads, p.Quad)
	// remove primitive
	delete(qs.prim, id)
	di := -1
	for i, p2 := range qs.all {
		if p == p2 {
			di = i
			break
		}
	}
	if di >= 0 {
		if !qs.reading {
			qs.all = append(qs.all[:di], qs.all[di+1:]...)
		} else {
			all := make([]*Primitive, 0, len(qs.all)-1)
			all = append(all, qs.all[:di]...)
			all = append(all, qs.all[di+1:]...)
			qs.all = all
			qs.reading = false // this is a new slice
		}
	}
	qs.deleteQuadNodes(p.Quad)
	return true
}

func (qs *QuadStore) findQuad(q quad.Quad) (int64, internalQuad, bool) {
	p, ok := qs.resolveQuad(q, false)
	if !ok {
		return 0, p, false
	}
	id := qs.quads[p]
	return id, p, id != 0
}

func (qs *QuadStore) ApplyDeltas(epoch int64, deltas []graph.Delta, ignoreOpts graph.IgnoreOpts) error {
	for _, d := range deltas {
		if (epoch != graph.NoCidEpoch && len(d.Cid) == 0) ||
			(epoch == graph.NoCidEpoch && len(d.Cid) > 0) {
			return &graph.DeltaError{Delta: d, Err: graph.ErrInvalidCid}
		}
	}
	// Precheck the whole transaction (if required)
	if !ignoreOpts.IgnoreDup || !ignoreOpts.IgnoreMissing {
		for _, d := range deltas {
			switch d.Action {
			case graph.Add:
				if !ignoreOpts.IgnoreDup {
					if _, _, ok := qs.findQuad(d.Quad); ok {
						return &graph.DeltaError{Delta: d, Err: graph.ErrQuadExists}
					}
				}
			case graph.Delete:
				if !ignoreOpts.IgnoreMissing {
					if len(d.Cid) > 0 {
						if _, ok := qs.cqIndex.Get(d.Cid); !ok {
							return &graph.DeltaError{Delta: d, Err: graph.ErrQuadIndexNotExist}
						}
					} else if _, _, ok := qs.findQuad(d.Quad); !ok {
						return &graph.DeltaError{Delta: d, Err: graph.ErrQuadNotExist}
					}
				}
			default:
				return &graph.DeltaError{Delta: d, Err: graph.ErrInvalidAction}
			}
		}
	}

	for _, d := range deltas {
		switch d.Action {
		case graph.Add:
			id, _ := qs.AddQuad(d.Quad)
			if len(d.Cid) > 0 {
				qs.cqIndex.Add(d.Cid, id)
			}
		case graph.Delete:
			if len(d.Cid) > 0 {
				if id, ok := qs.cqIndex.Get(d.Cid); ok {
					qs.Delete(id)
					qs.cqIndex.Delete(d.Cid)
				}
			} else if id, _, ok := qs.findQuad(d.Quad); ok {
				qs.Delete(id)
			}
		default:
			// TODO: ideally we should rollback it
			return &graph.DeltaError{Delta: d, Err: graph.ErrInvalidAction}
		}
	}
	if epoch > 0 {
		qs.epoch = epoch
	}
	qs.horizon++ //
	return nil
}

func asID(v graph.Ref) (int64, bool) {
	switch v := v.(type) {
	case bnode:
		return int64(v), true
	case qprim:
		return v.p.ID, true
	default:
		return 0, false
	}
}

func (qs *QuadStore) quad(v graph.Ref) (q internalQuad, ok bool) {
	switch v := v.(type) {
	case bnode:
		p := qs.prim[int64(v)]
		if p == nil {
			return
		}
		q = p.Quad
	case qprim:
		q = v.p.Quad
	default:
		return internalQuad{}, false
	}
	return q, !q.Zero()
}

func (qs *QuadStore) Quad(index graph.Ref) quad.Quad {
	q, ok := qs.quad(index)
	if !ok {
		return quad.Quad{}
	}
	return qs.lookupQuadDirs(q)
}

func (qs *QuadStore) QuadIterator(d quad.Direction, value graph.Ref) iterator.Shape {
	id, ok := asID(value)
	if !ok {
		return iterator.NewNull()
	}
	index, ok := qs.index.Get(d, id)
	if ok && index.Len() != 0 {
		return qs.newIterator(index, d, id)
	}
	return iterator.NewNull()
}

func (qs *QuadStore) QuadIteratorSize(ctx context.Context, d quad.Direction, v graph.Ref) (refs.Size, error) {
	id, ok := asID(v)
	if !ok {
		return refs.Size{Value: 0, Exact: true}, nil
	}
	index, ok := qs.index.Get(d, id)
	if !ok {
		return refs.Size{Value: 0, Exact: true}, nil
	}
	return refs.Size{Value: int64(index.Len()), Exact: true}, nil
}

func (qs *QuadStore) Stats(ctx context.Context, exact bool) (graph.Stats, error) {
	return graph.Stats{
		Nodes: refs.Size{
			Value: int64(len(qs.vals)),
			Exact: true,
		},
		Quads: refs.Size{
			Value: int64(len(qs.quads)),
			Exact: true,
		},
		Epoch: qs.epoch,
	}, nil
}

func (qs *QuadStore) ValueOf(name quad.Value) graph.Ref {
	if name == nil {
		return nil
	}
	id := qs.vals[name.String()]
	if id == 0 {
		return nil
	}
	return bnode(id)
}

func (qs *QuadStore) NameOf(v graph.Ref) quad.Value {
	if v == nil {
		return nil
	} else if v, ok := v.(refs.PreFetchedValue); ok {
		return v.NameOf()
	}
	n, ok := asID(v)
	if !ok {
		return nil
	}
	if _, ok = qs.prim[n]; !ok {
		return nil
	}
	return qs.lookupVal(n)
}

func (qs *QuadStore) QuadsAllIterator() iterator.Shape {
	return qs.newAllIterator(false, qs.last)
}

func (qs *QuadStore) QuadDirection(val graph.Ref, d quad.Direction) graph.Ref {
	q, ok := qs.quad(val)
	if !ok {
		return nil
	}
	id := q.Dir(d)
	if id == 0 {
		return nil
	}
	return bnode(id)
}

func (qs *QuadStore) NodesAllIterator() iterator.Shape {
	return qs.newAllIterator(true, qs.last)
}

func (qs *QuadStore) Close() error { return nil }

func (qs *QuadStore) SyncToSearcher(ctx context.Context, s graph.Searcher) {
	val, err := s.GetMeta("maxid")
	if err != nil {
		clog.Warningf("[memstore] Error getting searcher meta: %v", err)
		return
	}
	fv, ok := val.(float64)
	if !ok {
		clog.Warningf("[memstore] Unexpected maxid: %T", val)
		return
	}

	start := int64(fv)
	cur := start + 1
	clog.Infof("[kv] Update searcher from %d to %d", cur, qs.last)
	for ; cur <= qs.last; cur++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		v := qs.lookupVal(cur)
		if v != nil {
			node := graph.Node{
				ID: cur,
			}
			switch v.(type) {
			case quad.IRI:
				node.Type = "iri"
				node.Content = v.String()
			case quad.BNode:
				node.Type = "bnode"
				node.Content = v.String()
			case quad.String:
				node.Type = "string"
				node.Content = v.Native().(string)
				if node.NotIndex() {
					continue
				}
			default:
				clog.Warningf("ignore unexpected value type: %T", v)
				continue
			}
			err = s.IndexNodes([]graph.Node{node})
			if err != nil {
				data, _ := json.Marshal(node)
				clog.Warningf("[memstore] Error indexing node `%s`: %s", string(data), err)
				return
			}
		}

		err = s.SetMeta(map[string]interface{}{
			"maxid": cur,
		})
		if err != nil {
			clog.Warningf("[memstore] Error updating meta(maxid=%d): %s", cur, err)
			return
		}

		if (cur-start)%500 == 0 {
			clog.Infof("[memstore] Update searcher to id: %d", cur)
		}
	}
	clog.Infof("[memstore] Update searcher to id: %d", cur-1)
}
