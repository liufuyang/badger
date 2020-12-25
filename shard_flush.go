package badger

import (
	"github.com/pingcap/badger/fileutil"
	"github.com/pingcap/badger/protos"
	"github.com/pingcap/badger/table/memtable"
	"github.com/pingcap/badger/table/sstable"
	"github.com/pingcap/badger/y"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"os"
	"sync/atomic"
	"unsafe"
)

type shardFlushTask struct {
	shard        *Shard
	tbl          *memtable.CFTable
	commitTS     uint64
	splittingIdx int
	splitting    bool
}

func (sdb *ShardingDB) runFlushMemTable(c *y.Closer) {
	defer c.Done()
	for task := range sdb.flushCh {
		fd, err := sdb.createL0File(task.tbl.ID())
		if err != nil {
			panic(err)
		}
		err = sdb.flushMemTable(task, fd)
		if err != nil {
			panic(err)
		}
		filename := fd.Name()
		fd.Close()
		if sdb.s3c != nil {
			err = putSSTBuildResultToS3(sdb.s3c, &sstable.BuildResult{FileName: filename})
			if err != nil {
				// TODO: handle this error by queue the failed operation and retry.
				panic(err)
			}
		}
		l0Table, err := openShardL0Table(filename, task.tbl.ID())
		if err != nil {
			panic(err)
		}
		err = sdb.addShardL0Table(task, l0Table)
		if err != nil {
			panic(err)
		}
	}
}

func (sdb *ShardingDB) flushMemTable(task *shardFlushTask, fd *os.File) error {
	m := task.tbl
	log.S().Info("flush memtable")
	writer := fileutil.NewBufferedWriter(fd, sdb.opt.TableBuilderOptions.WriteBufferSize, nil)
	builder := newShardL0Builder(sdb.numCFs, task.commitTS, sdb.opt.TableBuilderOptions)
	for cf := 0; cf < sdb.numCFs; cf++ {
		it := m.NewIterator(cf, false)
		if it == nil {
			continue
		}
		for it.Rewind(); it.Valid(); y.NextAllVersion(it) {
			builder.Add(cf, it.Key(), it.Value())
		}
	}
	shardL0Data := builder.Finish()
	_, err := writer.Write(shardL0Data)
	if err != nil {
		return err
	}
	return writer.Finish()
}

func (sdb *ShardingDB) addShardL0Table(task *shardFlushTask, l0 *shardL0Table) error {
	shard := task.shard
	change := newManifestChange(l0.fid, shard.ID, -1, 0, protos.ManifestChange_CREATE)
	keysMap := newFileMetaKeysMap()
	keysMap.addFromShardL0Tables([]*shardL0Table{l0})
	err := sdb.manifest.addChanges(keysMap, change)
	if err != nil {
		if errors.Cause(err) != errShardNotFound {
			return err
		}
		var shardStartKey []byte
		if task.splittingIdx == 0 {
			shardStartKey = task.shard.Start
		} else {
			shardStartKey = task.shard.splitKeys[task.splittingIdx-1]
		}
		shard = sdb.loadShardTree().get(shardStartKey)
		change = newManifestChange(l0.fid, shard.ID, -1, 0, protos.ManifestChange_CREATE)
		err = sdb.manifest.addChanges(keysMap, change)
		if err != nil {
			return err
		}
	}
	oldL0sPtr := shard.l0s
	oldMemTblsPtr := shard.memTbls
	if task.splitting {
		oldL0sPtr = shard.splittingL0s[task.splittingIdx]
		oldMemTblsPtr = shard.splittingMemTbls[task.splittingIdx]
	}
	oldL0Tbls := (*shardL0Tables)(atomic.LoadPointer(oldL0sPtr))
	newL0Tbls := &shardL0Tables{make([]*shardL0Table, 0, len(oldL0Tbls.tables)+1)}
	newL0Tbls.tables = append(newL0Tbls.tables, l0)
	newL0Tbls.tables = append(newL0Tbls.tables, oldL0Tbls.tables...)
	y.Assert(atomic.CompareAndSwapPointer(oldL0sPtr, unsafe.Pointer(oldL0Tbls), unsafe.Pointer(newL0Tbls)))
	shard.addEstimatedSize(l0.size)
	for {
		oldMemTbls := (*shardingMemTables)(atomic.LoadPointer(oldMemTblsPtr))
		newMemTbls := &shardingMemTables{tables: make([]*memtable.CFTable, len(oldMemTbls.tables)-1)}
		copy(newMemTbls.tables, oldMemTbls.tables)
		if atomic.CompareAndSwapPointer(oldMemTblsPtr, unsafe.Pointer(oldMemTbls), unsafe.Pointer(newMemTbls)) {
			break
		}
	}
	return nil
}
