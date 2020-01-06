package indexheader

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb/fileutil"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/runutil"
)

const (
	// JSONVersion1 is a enumeration of index-cache.json versions supported by Thanos.
	JSONVersion1 = iota + 1
)

var (
	jsonUnmarshalError = errors.New("unmarshal index cache")
)

type postingsRange struct {
	Name, Value string
	Start, End  int64
}

type indexCache struct {
	Version      int
	CacheVersion int
	Symbols      map[uint32]string
	LabelValues  map[string][]string
	Postings     []postingsRange
}

type realByteSlice []byte

func (b realByteSlice) Len() int {
	return len(b)
}

func (b realByteSlice) Range(start, end int) []byte {
	return b[start:end]
}

func (b realByteSlice) Sub(start, end int) index.ByteSlice {
	return b[start:end]
}

func getSymbolTable(b index.ByteSlice) (map[uint32]string, error) {
	version := int(b.Range(4, 5)[0])

	if version != 1 && version != 2 {
		return nil, errors.Errorf("unknown index file version %d", version)
	}

	toc, err := index.NewTOCFromByteSlice(b)
	if err != nil {
		return nil, errors.Wrap(err, "read TOC")
	}

	symbolsV2, symbolsV1, err := index.ReadSymbols(b, version, int(toc.Symbols))
	if err != nil {
		return nil, errors.Wrap(err, "read symbols")
	}

	symbolsTable := make(map[uint32]string, len(symbolsV1)+len(symbolsV2))
	for o, s := range symbolsV1 {
		symbolsTable[o] = s
	}
	for o, s := range symbolsV2 {
		symbolsTable[uint32(o)] = s
	}

	return symbolsTable, nil
}

// WriteJSON writes a cache file containing the first lookup stages
// for an index file.
func WriteJSON(logger log.Logger, indexFn string, fn string) error {
	indexFile, err := fileutil.OpenMmapFile(indexFn)
	if err != nil {
		return errors.Wrapf(err, "open mmap index file %s", indexFn)
	}
	defer runutil.CloseWithLogOnErr(logger, indexFile, "close index cache mmap file from %s", indexFn)

	b := realByteSlice(indexFile.Bytes())
	indexr, err := index.NewReader(b)
	if err != nil {
		return errors.Wrap(err, "open index reader")
	}
	defer runutil.CloseWithLogOnErr(logger, indexr, "load index cache reader")

	// We assume reader verified index already.
	symbols, err := getSymbolTable(b)
	if err != nil {
		return err
	}

	f, err := os.Create(fn)
	if err != nil {
		return errors.Wrap(err, "create index cache file")
	}
	defer runutil.CloseWithLogOnErr(logger, f, "index cache writer")

	v := indexCache{
		Version:      indexr.Version(),
		CacheVersion: JSONVersion1,
		Symbols:      symbols,
		LabelValues:  map[string][]string{},
	}

	// Extract label value indices.
	lnames, err := indexr.LabelIndices()
	if err != nil {
		return errors.Wrap(err, "read label indices")
	}
	for _, lns := range lnames {
		if len(lns) != 1 {
			continue
		}
		ln := lns[0]

		tpls, err := indexr.LabelValues(ln)
		if err != nil {
			return errors.Wrap(err, "get label values")
		}
		vals := make([]string, 0, tpls.Len())

		for i := 0; i < tpls.Len(); i++ {
			v, err := tpls.At(i)
			if err != nil {
				return errors.Wrap(err, "get label value")
			}
			if len(v) != 1 {
				return errors.Errorf("unexpected tuple length %d", len(v))
			}
			vals = append(vals, v[0])
		}
		v.LabelValues[ln] = vals
	}

	// Extract postings ranges.
	pranges, err := indexr.PostingsRanges()
	if err != nil {
		return errors.Wrap(err, "read postings ranges")
	}
	for l, rng := range pranges {
		v.Postings = append(v.Postings, postingsRange{
			Name:  l.Name,
			Value: l.Value,
			Start: rng.Start,
			End:   rng.End,
		})
	}

	if err := json.NewEncoder(f).Encode(&v); err != nil {
		return errors.Wrap(err, "encode file")
	}
	return nil
}

// JSONReader is a reader based on index-cache.json files.
type JSONReader struct {
	indexVersion int
	symbols      []string
	lvals        map[string][]string
	postings     map[labels.Label]index.Range
}

func NewJSONReader(ctx context.Context, logger log.Logger, bkt objstore.BucketReader, dir string, id ulid.ULID) (*JSONReader, error) {
	cachefn := filepath.Join(dir, id.String(), block.IndexCacheFilename)
	jr, err := newJSONReaderFromFile(logger, cachefn)
	if err == nil {
		return jr, err
	}

	if !os.IsNotExist(errors.Cause(err)) && errors.Cause(err) != jsonUnmarshalError {
		return nil, errors.Wrap(err, "read index cache")
	}

	// Try to download index cache file from object store.
	if err = objstore.DownloadFile(ctx, logger, bkt, filepath.Join(id.String(), block.IndexCacheFilename), cachefn); err == nil {
		return newJSONReaderFromFile(logger, cachefn)
	}

	if !bkt.IsObjNotFoundErr(errors.Cause(err)) && errors.Cause(err) != jsonUnmarshalError {
		return nil, errors.Wrap(err, "download index cache file")
	}

	// No cache exists on disk yet, build it from the downloaded index and retry.
	fn := filepath.Join(dir, id.String(), block.IndexFilename)

	if err := objstore.DownloadFile(ctx, logger, bkt, filepath.Join(id.String(), block.IndexFilename), fn); err != nil {
		return nil, errors.Wrap(err, "download index file")
	}

	defer func() {
		if rerr := os.Remove(fn); rerr != nil {
			level.Error(logger).Log("msg", "failed to remove temp index file", "path", fn, "err", rerr)
		}
	}()

	if err := WriteJSON(logger, fn, cachefn); err != nil {
		return nil, errors.Wrap(err, "write index cache")
	}

	return newJSONReaderFromFile(logger, cachefn)
}

// ReadJSON reads an index cache file.
func newJSONReaderFromFile(logger log.Logger, fn string) (*JSONReader, error) {
	f, err := os.Open(fn)
	if err != nil {
		return nil, errors.Wrap(err, "open file")
	}
	defer runutil.CloseWithLogOnErr(logger, f, "index reader")

	var v indexCache

	bytes, err := ioutil.ReadFile(fn)
	if err != nil {
		return nil, errors.Wrap(err, "read file")
	}

	if err = json.Unmarshal(bytes, &v); err != nil {
		return nil, errors.Wrap(jsonUnmarshalError, err.Error())
	}

	strs := map[string]string{}
	var maxSymbolID uint32
	for o := range v.Symbols {
		if o > maxSymbolID {
			maxSymbolID = o
		}
	}

	jr := &JSONReader{
		indexVersion: v.Version,
		lvals:        make(map[string][]string, len(v.LabelValues)),
		postings:     make(map[labels.Label]index.Range, len(v.Postings)),
		symbols:      make([]string, maxSymbolID+1),
	}

	// Most strings we encounter are duplicates. Dedup string objects that we keep
	// around after the function returns to reduce total memory usage.
	// NOTE(fabxc): it could even make sense to deduplicate globally.
	getStr := func(s string) string {
		if cs, ok := strs[s]; ok {
			return cs
		}
		strs[s] = s
		return s
	}

	for o, s := range v.Symbols {
		jr.symbols[o] = getStr(s)
	}
	for ln, vals := range v.LabelValues {
		for i := range vals {
			vals[i] = getStr(vals[i])
		}
		jr.lvals[getStr(ln)] = vals
	}
	for _, e := range v.Postings {
		l := labels.Label{
			Name:  getStr(e.Name),
			Value: getStr(e.Value),
		}
		jr.postings[l] = index.Range{Start: e.Start, End: e.End}
	}
	return jr, nil
}

func (r *JSONReader) IndexVersion() int {
	return r.indexVersion
}

func (r *JSONReader) LookupSymbol(o uint32) (string, error) {
	idx := int(o)
	if idx >= len(r.symbols) {
		return "", errors.Errorf("bucketIndexReader: unknown symbol offset %d", o)
	}

	return r.symbols[idx], nil
}

func (r *JSONReader) PostingsOffset(name, value string) index.Range {
	return r.postings[labels.Label{Name: name, Value: value}]
}

// LabelValues returns label values for single name.
func (r *JSONReader) LabelValues(name string) []string {
	res := make([]string, 0, len(r.lvals[name]))
	return append(res, r.lvals[name]...)
}

// LabelNames returns a list of label names.
func (r *JSONReader) LabelNames() []string {
	res := make([]string, 0, len(r.lvals))
	for ln := range r.lvals {
		res = append(res, ln)
	}
	sort.Strings(res)
	return res
}
