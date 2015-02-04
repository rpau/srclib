package store

import (
	"fmt"
	"io"
	"io/ioutil"

	"github.com/alecthomas/binary"
	"github.com/smartystreets/mafsa"

	"strings"

	"sort"

	"sourcegraph.com/sourcegraph/srclib/graph"
)

type defQueryIndex struct {
	mt    *mafsaTable
	f     DefFilter
	ready bool
}

var _ interface {
	Index
	persistedIndex
	defIndexBuilder
	defIndex
} = (*defQueryIndex)(nil)

var c_defQueryIndex_getByQuery = 0 // counter

func (x *defQueryIndex) String() string { return fmt.Sprintf("defQueryIndex(ready=%v)", x.ready) }

func (x *defQueryIndex) getByQuery(q string) (byteOffsets, bool) {
	vlog.Printf("defQueryIndex.getByQuery(%q)", q)
	c_defQueryIndex_getByQuery++

	if x.mt == nil {
		panic("mafsaTable not built/read")
	}

	q = strings.ToLower(q)
	node, i := x.mt.t.IndexedTraverse([]rune(q))
	if node == nil {
		return nil, false
	}
	nn := node.Number
	if node.Final {
		i--
		nn++
	}
	var ofs byteOffsets
	for _, ofs0 := range x.mt.Values[i : i+nn] {
		ofs = append(ofs, ofs0...)
	}
	vlog.Printf("defQueryIndex.getByQuery(%q): found %d defs.", q, len(ofs))
	return ofs, true
}

// Covers implements defIndex.
func (x *defQueryIndex) Covers(filters interface{}) int {
	cov := 0
	for _, f := range storeFilters(filters) {
		if _, ok := f.(ByDefQueryFilter); ok {
			cov++
		}
	}
	return cov
}

// Defs implements defIndex.
func (x *defQueryIndex) Defs(f ...DefFilter) (byteOffsets, error) {
	for _, ff := range f {
		if pf, ok := ff.(ByDefQueryFilter); ok {
			ofs, found := x.getByQuery(pf.ByDefQuery())
			if !found {
				return nil, nil
			}
			return ofs, nil
		}
	}
	return nil, nil
}

type defLowerNameAndOffset struct {
	lowerName string
	ofs       int64
}

type defsByLowerName []*defLowerNameAndOffset

func (ds defsByLowerName) Len() int           { return len(ds) }
func (ds defsByLowerName) Swap(i, j int)      { ds[i], ds[j] = ds[j], ds[i] }
func (ds defsByLowerName) Less(i, j int) bool { return ds[i].lowerName < ds[j].lowerName }

// Build implements defIndexBuilder.
func (x *defQueryIndex) Build(defs []*graph.Def, ofs byteOffsets) error {
	vlog.Printf("defQueryIndex: building index... (%d defs)", len(defs))

	// Clone slice so we can sort it by whatever we want.
	dofs := make([]*defLowerNameAndOffset, 0, len(defs))
	for i, def := range defs {
		if x.f.SelectDef(def) {
			dofs = append(dofs, &defLowerNameAndOffset{strings.ToLower(def.Name), ofs[i]})
		}
	}
	if len(dofs) == 0 {
		x.mt = &mafsaTable{}
		x.ready = true
		return nil
	}
	sort.Sort(defsByLowerName(dofs))
	vlog.Printf("defQueryIndex: done sorting by def name (%d defs).", len(defs))

	bt := mafsa.New()
	x.mt = &mafsaTable{}
	x.mt.Values = make([]byteOffsets, 0, len(dofs))
	j := 0 // index of earliest def with same name
	for i, def := range dofs {
		if i > 0 && dofs[j].lowerName == def.lowerName {
			x.mt.Values[len(x.mt.Values)-1] = append(x.mt.Values[len(x.mt.Values)-1], def.ofs)
		} else {
			bt.Insert(def.lowerName)
			x.mt.Values = append(x.mt.Values, byteOffsets{def.ofs})
			j = i
		}
	}
	bt.Finish()
	vlog.Printf("defQueryIndex: done adding %d defs to MAFSA & table and minimizing.", len(defs))

	b, err := bt.MarshalBinary()
	if err != nil {
		return err
	}
	vlog.Printf("defQueryIndex: done serializing MAFSA & table to %d bytes.", len(b))

	x.mt.B = b
	x.mt.t, err = new(mafsa.Decoder).Decode(x.mt.B)
	if err != nil {
		return err
	}
	x.ready = true
	vlog.Printf("defQueryIndex: done building index (%d defs).", len(defs))
	return nil
}

// Write implements persistedIndex.
func (x *defQueryIndex) Write(w io.Writer) error {
	if x.mt == nil {
		panic("no mafsaTable to write")
	}
	b, err := binary.Marshal(x.mt)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Read implements persistedIndex.
func (x *defQueryIndex) Read(r io.Reader) error {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	var mt mafsaTable
	err = binary.Unmarshal(b, &mt)
	x.mt = &mt
	if err == nil && len(x.mt.B) > 0 {
		x.mt.t, err = new(mafsa.Decoder).Decode(x.mt.B)
	}
	x.ready = (err == nil)
	return err
}

// Ready implements persistedIndex.
func (x *defQueryIndex) Ready() bool { return x.ready }

// A mafsaTable is a minimal perfect hashed MA-FSA with an associated
// table of values for each entry in the MA-FSA (indexed on the
// entry's hash value).
type mafsaTable struct {
	t      *mafsa.MinTree
	B      []byte        // bytes of the MinTree
	Values []byteOffsets // one value per entry in build or min
}
