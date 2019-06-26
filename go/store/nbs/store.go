// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package nbs

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/liquidata-inc/ld/dolt/go/store/blobstore"

	"cloud.google.com/go/storage"
	"github.com/dustin/go-humanize"
	"github.com/liquidata-inc/ld/dolt/go/store/chunks"
	"github.com/liquidata-inc/ld/dolt/go/store/constants"
	"github.com/liquidata-inc/ld/dolt/go/store/d"
	"github.com/liquidata-inc/ld/dolt/go/store/hash"
)

var ErrFetchFailure = errors.New("fetch failed")

// The root of a Noms Chunk Store is stored in a 'manifest', along with the
// names of the tables that hold all the chunks in the store. The number of
// chunks in each table is also stored in the manifest.

const (
	// StorageVersion is the version of the on-disk Noms Chunks Store data format.
	StorageVersion = "4"

	defaultMemTableSize uint64 = (1 << 20) * 128 // 128MB
	defaultMaxTables           = 256

	defaultIndexCacheSize    = (1 << 20) * 8 // 8MB
	defaultManifestCacheSize = 1 << 23       // 8MB
	preflushChunkCount       = 8
)

var (
	cacheOnce           = sync.Once{}
	globalIndexCache    *indexCache
	makeManifestManager func(manifest) manifestManager
	globalFDCache       *fdCache
)

func makeGlobalCaches() {
	globalIndexCache = newIndexCache(defaultIndexCacheSize)
	globalFDCache = newFDCache(defaultMaxTables)

	manifestCache := newManifestCache(defaultManifestCacheSize)
	manifestLocks := newManifestLocks()
	makeManifestManager = func(m manifest) manifestManager { return manifestManager{m, manifestCache, manifestLocks} }
}

type NomsBlockStore struct {
	mm manifestManager
	p  tablePersister
	c  conjoiner

	mu       sync.RWMutex // protects the following state
	mt       *memTable
	tables   tableSet
	upstream manifestContents

	mtSize   uint64
	putCount uint64

	stats *Stats
}

type Range struct {
	Offset uint64
	Length uint32
}

func (nbs *NomsBlockStore) GetChunkLocations(hashes hash.HashSet) map[hash.Hash]map[hash.Hash]Range {
	gr := toGetRecords(hashes)

	ranges := make(map[hash.Hash]map[hash.Hash]Range)
	f := func(css chunkSources) {
		for _, cs := range css {
			switch tr := cs.(type) {
			case *mmapTableReader:
				offsetRecSlice, _ := tr.findOffsets(gr)
				if len(offsetRecSlice) > 0 {
					y, ok := ranges[hash.Hash(tr.h)]

					if !ok {
						y = make(map[hash.Hash]Range)
					}

					for _, offsetRec := range offsetRecSlice {
						ord := offsetRec.ordinal
						length := tr.lengths[ord]
						h := hash.Hash(*offsetRec.a)
						y[h] = Range{Offset: offsetRec.offset, Length: length}

						delete(hashes, h)
					}

					if len(offsetRecSlice) > 0 {
						gr = toGetRecords(hashes)
					}

					ranges[hash.Hash(tr.h)] = y
				}
			case *chunkSourceAdapter:
				y, ok := ranges[hash.Hash(tr.h)]

				if !ok {
					y = make(map[hash.Hash]Range)
				}

				tableIndex := tr.index()
				var foundHashes []hash.Hash
				for h := range hashes {
					ord := tableIndex.lookupOrdinal(addr(h))

					if ord < tableIndex.chunkCount {
						foundHashes = append(foundHashes, h)
						y[h] = Range{Offset: tableIndex.offsets[ord], Length: tableIndex.lengths[ord]}
					}
				}

				ranges[hash.Hash(tr.h)] = y

				for _, h := range foundHashes {
					delete(hashes, h)
				}

			default:
				panic(reflect.TypeOf(cs))
			}
		}
	}

	f(nbs.tables.upstream)
	f(nbs.tables.novel)

	return ranges
}

func (nbs *NomsBlockStore) UpdateManifest(ctx context.Context, updates map[hash.Hash]uint32) (ManifestInfo, error) {
	nbs.mm.LockForUpdate()
	defer nbs.mm.UnlockForUpdate()

	nbs.mu.Lock()
	defer nbs.mu.Unlock()

	var stats Stats
	ok, contents, err := nbs.mm.Fetch(ctx, &stats)

	if err != nil {
		return manifestContents{}, err
	} else if !ok {
		contents = manifestContents{vers: constants.NomsVersion}
	}

	currSpecs := make(map[addr]bool)
	for _, spec := range contents.specs {
		currSpecs[spec.name] = true
	}

	var addCount int
	for h, count := range updates {
		a := addr(h)

		if _, ok := currSpecs[a]; !ok {
			addCount++
			contents.specs = append(contents.specs, tableSpec{a, count})
		}
	}

	if addCount == 0 {
		return contents, nil
	}

	updatedContents, err := nbs.mm.Update(ctx, contents.lock, contents, &stats, nil)

	if err != nil {
		return manifestContents{}, err
	}

	nbs.upstream = updatedContents
	nbs.tables = nbs.tables.Rebase(ctx, contents.specs, nbs.stats)

	return updatedContents, nil
}

func NewAWSStore(ctx context.Context, table, ns, bucket string, s3 s3svc, ddb ddbsvc, memTableSize uint64) *NomsBlockStore {
	cacheOnce.Do(makeGlobalCaches)
	readRateLimiter := make(chan struct{}, 32)
	p := &awsTablePersister{
		s3,
		bucket,
		readRateLimiter,
		nil,
		&ddbTableStore{ddb, table, readRateLimiter, nil},
		awsLimits{defaultS3PartSize, minS3PartSize, maxS3PartSize, maxDynamoItemSize, maxDynamoChunks},
		globalIndexCache,
		ns,
	}
	mm := makeManifestManager(newDynamoManifest(table, ns, ddb))
	return newNomsBlockStore(ctx, mm, p, inlineConjoiner{defaultMaxTables}, memTableSize)
}

// NewGCSStore returns an nbs implementation backed by a GCSBlobstore
func NewGCSStore(ctx context.Context, bucketName, path string, gcs *storage.Client, memTableSize uint64) *NomsBlockStore {
	cacheOnce.Do(makeGlobalCaches)

	bucket := gcs.Bucket(bucketName)
	bs := blobstore.NewGCSBlobstore(bucket, path)
	mm := makeManifestManager(blobstoreManifest{"manifest", bs})

	p := &blobstorePersister{bs, s3BlockSize, globalIndexCache}
	return newNomsBlockStore(ctx, mm, p, inlineConjoiner{defaultMaxTables}, memTableSize)
}

func NewLocalStore(ctx context.Context, dir string, memTableSize uint64) *NomsBlockStore {
	cacheOnce.Do(makeGlobalCaches)
	d.PanicIfError(checkDir(dir))

	mm := makeManifestManager(fileManifest{dir})
	p := newFSTablePersister(dir, globalFDCache, globalIndexCache)
	return newNomsBlockStore(ctx, mm, p, inlineConjoiner{defaultMaxTables}, memTableSize)
}

func checkDir(dir string) error {
	stat, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !stat.IsDir() {
		return fmt.Errorf("path is not a directory: %s", dir)
	}
	return nil
}

func newNomsBlockStore(ctx context.Context, mm manifestManager, p tablePersister, c conjoiner, memTableSize uint64) *NomsBlockStore {
	if memTableSize == 0 {
		memTableSize = defaultMemTableSize
	}

	nbs := &NomsBlockStore{
		mm:       mm,
		p:        p,
		c:        c,
		tables:   newTableSet(p),
		upstream: manifestContents{vers: constants.NomsVersion},
		mtSize:   memTableSize,
		stats:    NewStats(),
	}

	t1 := time.Now()
	defer nbs.stats.OpenLatency.SampleTimeSince(t1)

	exists, contents, err := nbs.mm.Fetch(ctx, nbs.stats)

	// TODO: fix panics
	d.PanicIfError(err)

	if exists {
		nbs.upstream = contents
		nbs.tables = nbs.tables.Rebase(ctx, contents.specs, nbs.stats)
	}

	return nbs
}

func newNomsBlockStoreWithContents(ctx context.Context, mm manifestManager, mc manifestContents, p tablePersister, c conjoiner, memTableSize uint64) *NomsBlockStore {
	if memTableSize == 0 {
		memTableSize = defaultMemTableSize
	}
	stats := NewStats()
	return &NomsBlockStore{
		mm:     mm,
		p:      p,
		c:      c,
		mtSize: memTableSize,
		stats:  stats,

		upstream: mc,
		tables:   newTableSet(p).Rebase(ctx, mc.specs, stats),
	}
}

func (nbs *NomsBlockStore) Put(ctx context.Context, c chunks.Chunk) {
	t1 := time.Now()
	a := addr(c.Hash())
	d.PanicIfFalse(nbs.addChunk(ctx, a, c.Data()))
	nbs.putCount++

	nbs.stats.PutLatency.SampleTimeSince(t1)
}

// TODO: figure out if there's a non-error reason for this to return false. If not, get rid of return value.
func (nbs *NomsBlockStore) addChunk(ctx context.Context, h addr, data []byte) bool {
	nbs.mu.Lock()
	defer nbs.mu.Unlock()
	if nbs.mt == nil {
		nbs.mt = newMemTable(nbs.mtSize)
	}
	if !nbs.mt.addChunk(h, data) {
		nbs.tables = nbs.tables.Prepend(ctx, nbs.mt, nbs.stats)
		nbs.mt = newMemTable(nbs.mtSize)
		return nbs.mt.addChunk(h, data)
	}
	return true
}

func (nbs *NomsBlockStore) Get(ctx context.Context, h hash.Hash) (chunks.Chunk, error) {
	t1 := time.Now()
	defer func() {
		nbs.stats.GetLatency.SampleTimeSince(t1)
		nbs.stats.ChunksPerGet.Sample(1)
	}()

	a := addr(h)
	data, tables := func() (data []byte, tables chunkReader) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		if nbs.mt != nil {
			data = nbs.mt.get(ctx, a, nbs.stats)
		}
		return data, nbs.tables
	}()

	if data != nil {
		return chunks.NewChunkWithHash(h, data), nil
	}

	if data := tables.get(ctx, a, nbs.stats); data != nil {
		return chunks.NewChunkWithHash(h, data), nil
	}

	return chunks.EmptyChunk, nil
}

func (nbs *NomsBlockStore) GetMany(ctx context.Context, hashes hash.HashSet, foundChunks chan *chunks.Chunk) error {
	t1 := time.Now()
	reqs := toGetRecords(hashes)

	defer func() {
		if len(hashes) > 0 {
			nbs.stats.GetLatency.SampleTimeSince(t1)
			nbs.stats.ChunksPerGet.Sample(uint64(len(reqs)))
		}
	}()

	wg := &sync.WaitGroup{}

	tables, remaining := func() (tables chunkReader, remaining bool) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		tables = nbs.tables
		remaining = true
		if nbs.mt != nil {
			remaining = nbs.mt.getMany(ctx, reqs, foundChunks, nil, nbs.stats)
		}

		return
	}()

	if remaining {
		tables.getMany(ctx, reqs, foundChunks, wg, nbs.stats)
		wg.Wait()
	}

	return nil
}

func toGetRecords(hashes hash.HashSet) []getRecord {
	reqs := make([]getRecord, len(hashes))
	idx := 0
	for h := range hashes {
		a := addr(h)
		reqs[idx] = getRecord{
			a:      &a,
			prefix: a.Prefix(),
		}
		idx++
	}

	sort.Sort(getRecordByPrefix(reqs))
	return reqs
}

func (nbs *NomsBlockStore) CalcReads(hashes hash.HashSet, blockSize uint64) (reads int, split bool) {
	reqs := toGetRecords(hashes)
	tables := func() (tables tableSet) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		tables = nbs.tables

		return
	}()

	reads, split, remaining := tables.calcReads(reqs, blockSize)
	d.Chk.False(remaining)
	return
}

func (nbs *NomsBlockStore) extractChunks(ctx context.Context, chunkChan chan<- *chunks.Chunk) {
	ch := make(chan extractRecord, 1)
	go func() {
		defer close(ch)
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		// Chunks in nbs.tables were inserted before those in nbs.mt, so extract chunks there _first_
		nbs.tables.extract(ctx, ch)
		if nbs.mt != nil {
			nbs.mt.extract(ctx, ch)
		}
	}()
	for rec := range ch {
		c := chunks.NewChunkWithHash(hash.Hash(rec.a), rec.data)
		chunkChan <- &c
	}
}

func (nbs *NomsBlockStore) Count() uint32 {
	count, tables := func() (count uint32, tables chunkReader) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		if nbs.mt != nil {
			count = nbs.mt.count()
		}
		return count, nbs.tables
	}()
	return count + tables.count()
}

func (nbs *NomsBlockStore) Has(ctx context.Context, h hash.Hash) bool {
	t1 := time.Now()
	defer func() {
		nbs.stats.HasLatency.SampleTimeSince(t1)
		nbs.stats.AddressesPerHas.Sample(1)
	}()

	a := addr(h)
	has, tables := func() (bool, chunkReader) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		return nbs.mt != nil && nbs.mt.has(a), nbs.tables
	}()
	has = has || tables.has(a)

	return has
}

func (nbs *NomsBlockStore) HasMany(ctx context.Context, hashes hash.HashSet) hash.HashSet {
	t1 := time.Now()

	reqs := toHasRecords(hashes)

	tables, remaining := func() (tables chunkReader, remaining bool) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		tables = nbs.tables

		remaining = true
		if nbs.mt != nil {
			remaining = nbs.mt.hasMany(reqs)
		}

		return
	}()

	if remaining {
		tables.hasMany(reqs)
	}

	if len(hashes) > 0 {
		nbs.stats.HasLatency.SampleTimeSince(t1)
		nbs.stats.AddressesPerHas.SampleLen(len(reqs))
	}

	absent := hash.HashSet{}
	for _, r := range reqs {
		if !r.has {
			absent.Insert(hash.New(r.a[:]))
		}
	}
	return absent
}

func toHasRecords(hashes hash.HashSet) []hasRecord {
	reqs := make([]hasRecord, len(hashes))
	idx := 0
	for h := range hashes {
		a := addr(h)
		reqs[idx] = hasRecord{
			a:      &a,
			prefix: a.Prefix(),
			order:  idx,
		}
		idx++
	}

	sort.Sort(hasRecordByPrefix(reqs))
	return reqs
}

func (nbs *NomsBlockStore) Rebase(ctx context.Context) {
	nbs.mu.Lock()
	defer nbs.mu.Unlock()
	exists, contents, err := nbs.mm.Fetch(ctx, nbs.stats)

	// TODO: fix panics
	d.PanicIfError(err)

	if exists {
		nbs.upstream = contents
		nbs.tables = nbs.tables.Rebase(ctx, contents.specs, nbs.stats)
	}
}

func (nbs *NomsBlockStore) Root(ctx context.Context) hash.Hash {
	nbs.mu.RLock()
	defer nbs.mu.RUnlock()
	return nbs.upstream.root
}

func (nbs *NomsBlockStore) Commit(ctx context.Context, current, last hash.Hash) (bool, error) {
	t1 := time.Now()
	defer nbs.stats.CommitLatency.SampleTimeSince(t1)

	anyPossiblyNovelChunks := func() bool {
		nbs.mu.Lock()
		defer nbs.mu.Unlock()
		return nbs.mt != nil || nbs.tables.Novel() > 0
	}

	if !anyPossiblyNovelChunks() && current == last {
		nbs.Rebase(ctx)
		return true, nil
	}

	func() {
		// This is unfortunate. We want to serialize commits to the same store
		// so that we avoid writing a bunch of unreachable small tables which result
		// from optismistic lock failures. However, this means that the time to
		// write tables is included in "commit" time and if all commits are
		// serialized, it means alot more waiting. Allow "non-trivial" tables to be
		// persisted outside of the commit-lock.
		nbs.mu.Lock()
		defer nbs.mu.Unlock()

		if nbs.mt != nil && nbs.mt.count() > preflushChunkCount {
			nbs.tables = nbs.tables.Prepend(ctx, nbs.mt, nbs.stats)
			nbs.mt = nil
		}
	}()

	nbs.mm.LockForUpdate()
	defer nbs.mm.UnlockForUpdate()
	for {
		if err := nbs.updateManifest(ctx, current, last); err == nil {
			return true, nil
		} else if err == errOptimisticLockFailedRoot || err == errLastRootMismatch {
			return false, nil
		} else if err != errOptimisticLockFailedTables {
			return false, err
		}

		// I guess this thing infinitely retries without backoff in the case off errOptimisticLockFailedTables
	}
}

var (
	errLastRootMismatch           = fmt.Errorf("last does not match nbs.Root()")
	errOptimisticLockFailedRoot   = fmt.Errorf("root moved")
	errOptimisticLockFailedTables = fmt.Errorf("tables changed")
)

func (nbs *NomsBlockStore) updateManifest(ctx context.Context, current, last hash.Hash) error {
	nbs.mu.Lock()
	defer nbs.mu.Unlock()
	if nbs.upstream.root != last {
		return errLastRootMismatch
	}

	handleOptimisticLockFailure := func(upstream manifestContents) error {
		nbs.upstream = upstream
		nbs.tables = nbs.tables.Rebase(ctx, upstream.specs, nbs.stats)

		if last != upstream.root {
			return errOptimisticLockFailedRoot
		}
		return errOptimisticLockFailedTables
	}

	if cached, doomed := nbs.mm.updateWillFail(nbs.upstream.lock); doomed {
		// Pre-emptive optimistic lock failure. Someone else in-process moved to the root, the set of tables, or both out from under us.
		return handleOptimisticLockFailure(cached)
	}

	if nbs.mt != nil && nbs.mt.count() > 0 {
		nbs.tables = nbs.tables.Prepend(ctx, nbs.mt, nbs.stats)
		nbs.mt = nil
	}

	if nbs.c.ConjoinRequired(nbs.tables) {
		nbs.upstream = nbs.c.Conjoin(ctx, nbs.upstream, nbs.mm, nbs.p, nbs.stats)
		nbs.tables = nbs.tables.Rebase(ctx, nbs.upstream.specs, nbs.stats)
		return errOptimisticLockFailedTables
	}

	specs := nbs.tables.ToSpecs()
	newContents := manifestContents{
		vers:  constants.NomsVersion,
		root:  current,
		lock:  generateLockHash(current, specs),
		specs: specs,
	}

	upstream, err := nbs.mm.Update(ctx, nbs.upstream.lock, newContents, nbs.stats, nil)
	if err != nil {
		return err
	}

	if newContents.lock != upstream.lock {
		// Optimistic lock failure. Someone else moved to the root, the set of tables, or both out from under us.
		return handleOptimisticLockFailure(upstream)
	}

	nbs.upstream = newContents
	nbs.tables = nbs.tables.Flatten()
	return nil
}

func (nbs *NomsBlockStore) Version() string {
	return nbs.upstream.vers
}

func (nbs *NomsBlockStore) Close() (err error) {
	return
}

func (nbs *NomsBlockStore) Stats() interface{} {
	return *nbs.stats
}

func (nbs *NomsBlockStore) StatsSummary() string {
	nbs.mu.Lock()
	defer nbs.mu.Unlock()

	return fmt.Sprintf("Root: %s; Chunk Count %d; Physical Bytes %s", nbs.upstream.root, nbs.tables.count(), humanize.Bytes(nbs.tables.physicalLen()))
}