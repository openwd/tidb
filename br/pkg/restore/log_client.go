// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/model"
	filter "github.com/pingcap/tidb-tools/pkg/table-filter"
	"github.com/pingcap/tidb/br/pkg/cdclog"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/kv"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/meta/autoid"
	titable "github.com/pingcap/tidb/table"
	"github.com/tikv/client-go/v2/oracle"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	tableLogPrefix = "t_"
	logPrefix      = "cdclog"

	metaFile      = "log.meta"
	ddlEventsDir  = "ddls"
	ddlFilePrefix = "ddl"

	maxUint64 = ^uint64(0)

	maxRetryTimes = 3
)

// concurrencyCfg set by user, which can adjust the restore performance.
type concurrencyCfg struct {
	BatchWriteKVPairs int
	BatchFlushKVPairs int
	BatchFlushKVSize  int64
	Concurrency       uint
	TCPConcurrency    int
	IngestConcurrency uint
}

// LogMeta represents the log.meta generated by cdc log backup.
type LogMeta struct {
	Names            map[int64]string `json:"names"`
	GlobalResolvedTS uint64           `json:"global_resolved_ts"`
}

// LogClient sends requests to restore files.
type LogClient struct {
	// lock DDL execution
	// TODO remove lock by using db session pool if necessary
	ddlLock sync.Mutex

	restoreClient  *Client
	splitClient    SplitClient
	importerClient ImporterClient

	// ingester is used to write and ingest kvs to tikv.
	// lightning has the simlar logic and can reuse it.
	ingester *Ingester

	// range of log backup
	startTS uint64
	endTS   uint64

	concurrencyCfg concurrencyCfg
	// meta info parsed from log backup
	meta         *LogMeta
	eventPullers map[int64]*cdclog.EventPuller
	tableBuffers map[int64]*cdclog.TableBuffer

	tableFilter filter.Filter

	// a map to store all drop schema ts, use it as a filter
	dropTSMap sync.Map
}

// NewLogRestoreClient returns a new LogRestoreClient.
func NewLogRestoreClient(
	ctx context.Context,
	restoreClient *Client,
	startTS uint64,
	endTS uint64,
	tableFilter filter.Filter,
	concurrency uint,
	batchFlushPairs int,
	batchFlushSize int64,
	batchWriteKVPairs int,
) (*LogClient, error) {
	var err error
	if endTS == 0 {
		// means restore all log data,
		// so we get current ts from restore cluster
		endTS, err = restoreClient.GetTS(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	tlsConf := restoreClient.GetTLSConfig()
	splitClient := NewSplitClient(restoreClient.GetPDClient(), tlsConf, false)
	importClient := NewImportClient(splitClient, tlsConf, restoreClient.keepaliveConf)

	cfg := concurrencyCfg{
		Concurrency:       concurrency,
		BatchFlushKVPairs: batchFlushPairs,
		BatchFlushKVSize:  batchFlushSize,
		BatchWriteKVPairs: batchWriteKVPairs,
		IngestConcurrency: concurrency * 16,
		TCPConcurrency:    int(concurrency) * 16,
	}

	// commitTS append into encode key. we use a unified ts for once log restore.
	commitTS := oracle.ComposeTS(time.Now().Unix()*1000, 0)
	lc := &LogClient{
		restoreClient:  restoreClient,
		splitClient:    splitClient,
		importerClient: importClient,
		startTS:        startTS,
		endTS:          endTS,
		concurrencyCfg: cfg,
		meta:           new(LogMeta),
		eventPullers:   make(map[int64]*cdclog.EventPuller),
		tableBuffers:   make(map[int64]*cdclog.TableBuffer),
		tableFilter:    tableFilter,
		ingester:       NewIngester(splitClient, cfg, commitTS, tlsConf),
	}
	return lc, nil
}

// ResetTSRange used for test.
func (l *LogClient) ResetTSRange(startTS uint64, endTS uint64) {
	l.startTS = startTS
	l.endTS = endTS
}

func (l *LogClient) maybeTSInRange(ts uint64) bool {
	// We choose the last event's ts as file name in cdclog when rotate.
	// so even this file name's ts is larger than l.endTS,
	// we still need to collect it, because it may have some events in this ts range.
	// TODO: find another effective filter to collect files
	return ts >= l.startTS
}

func (l *LogClient) tsInRange(ts uint64) bool {
	return l.startTS <= ts && ts <= l.endTS
}

func (l *LogClient) shouldFilter(item *cdclog.SortItem) bool {
	if val, ok := l.dropTSMap.Load(item.Schema); ok {
		if val.(uint64) > item.TS {
			return true
		}
	}
	return false
}

// NeedRestoreDDL determines whether to collect ddl file by ts range.
func (l *LogClient) NeedRestoreDDL(fileName string) (bool, error) {
	names := strings.Split(fileName, ".")
	if len(names) != 2 {
		log.Warn("found wrong format of ddl file", zap.String("file", fileName))
		return false, nil
	}
	if names[0] != ddlFilePrefix {
		log.Warn("file doesn't start with ddl", zap.String("file", fileName))
		return false, nil
	}
	ts, err := strconv.ParseUint(names[1], 10, 64)
	if err != nil {
		return false, errors.Trace(err)
	}

	// According to https://docs.aws.amazon.com/AmazonS3/latest/dev/ListingKeysUsingAPIs.html
	// list API return in UTF-8 binary order, so the cdc log create DDL file used
	// maxUint64 - the first DDL event's commit ts as the file name to return the latest ddl file.
	// see details at https://github.com/pingcap/ticdc/pull/826/files#diff-d2e98b3ed211b7b9bb7b6da63dd48758R81
	ts = maxUint64 - ts

	// In cdc, we choose the first event as the file name of DDL file.
	// so if the file ts is large than endTS, we can skip to execute it.
	// FIXME find a unified logic to filter row changes files and ddl files.
	if ts <= l.endTS {
		return true, nil
	}
	log.Info("filter ddl file by ts", zap.String("name", fileName), zap.Uint64("ts", ts))
	return false, nil
}

func (l *LogClient) collectDDLFiles(ctx context.Context) ([]string, error) {
	ddlFiles := make([]string, 0)
	opt := &storage.WalkOption{
		SubDir:    ddlEventsDir,
		ListCount: -1,
	}
	err := l.restoreClient.storage.WalkDir(ctx, opt, func(path string, size int64) error {
		fileName := filepath.Base(path)
		shouldRestore, err := l.NeedRestoreDDL(fileName)
		if err != nil {
			return errors.Trace(err)
		}
		if shouldRestore {
			ddlFiles = append(ddlFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, errors.Trace(err)
	}

	sort.Sort(sort.Reverse(sort.StringSlice(ddlFiles)))
	return ddlFiles, nil
}

func (l *LogClient) isDBRelatedDDL(ddl *cdclog.MessageDDL) bool {
	switch ddl.Type {
	case model.ActionDropSchema, model.ActionCreateSchema, model.ActionModifySchemaCharsetAndCollate:
		return true
	}
	return false
}

func (l *LogClient) isDropTable(ddl *cdclog.MessageDDL) bool {
	return ddl.Type == model.ActionDropTable
}

func (l *LogClient) doDBDDLJob(ctx context.Context, ddls []string) error {
	if len(ddls) == 0 {
		log.Info("no ddls to restore")
		return nil
	}

	for _, path := range ddls {
		data, err := l.restoreClient.storage.ReadFile(ctx, path)
		if err != nil {
			return errors.Trace(err)
		}
		eventDecoder, err := cdclog.NewJSONEventBatchDecoder(data)
		if err != nil {
			return errors.Trace(err)
		}
		for eventDecoder.HasNext() {
			item, err := eventDecoder.NextEvent(cdclog.DDL)
			if err != nil {
				return errors.Trace(err)
			}
			ddl := item.Data.(*cdclog.MessageDDL)
			log.Debug("[doDBDDLJob] parse ddl", zap.String("query", ddl.Query))
			if l.isDBRelatedDDL(ddl) && l.tsInRange(item.TS) {
				err = l.restoreClient.db.se.Execute(ctx, ddl.Query)
				if err != nil {
					log.Error("[doDBDDLJob] exec ddl failed",
						zap.String("query", ddl.Query), zap.Error(err))
					return errors.Trace(err)
				}
				if ddl.Type == model.ActionDropSchema {
					// store the drop schema ts, and then we need filter evetns which ts is small than this.
					l.dropTSMap.Store(item.Schema, item.TS)
				}
			}
		}
	}
	return nil
}

// NeedRestoreRowChange determine whether to collect this file by ts range.
func (l *LogClient) NeedRestoreRowChange(fileName string) (bool, error) {
	if fileName == logPrefix {
		// this file name appeared when file sink enabled
		return true, nil
	}
	names := strings.Split(fileName, ".")
	if len(names) != 2 {
		log.Warn("found wrong format of row changes file", zap.String("file", fileName))
		return false, nil
	}
	if names[0] != logPrefix {
		log.Warn("file doesn't start with row changes file", zap.String("file", fileName))
		return false, nil
	}
	ts, err := strconv.ParseUint(names[1], 10, 64)
	if err != nil {
		return false, errors.Trace(err)
	}
	if l.maybeTSInRange(ts) {
		return true, nil
	}
	log.Info("filter file by ts", zap.String("name", fileName), zap.Uint64("ts", ts))
	return false, nil
}

func (l *LogClient) collectRowChangeFiles(ctx context.Context) (map[int64][]string, error) {
	// we should collect all related tables row change files
	// by log meta info and by given table filter
	rowChangeFiles := make(map[int64][]string)

	// need collect restore tableIDs
	tableIDs := make([]int64, 0, len(l.meta.Names))

	// we need remove duplicate table name in collection.
	// when a table create and drop and create again.
	// then we will have two different table id with same tables.
	// we should keep the latest table id(larger table id), and filter the old one.
	nameIDMap := make(map[string]int64)
	for tableID, name := range l.meta.Names {
		if tid, ok := nameIDMap[name]; ok {
			if tid < tableID {
				nameIDMap[name] = tableID
			}
		} else {
			nameIDMap[name] = tableID
		}
	}
	for name, tableID := range nameIDMap {
		schema, table := ParseQuoteName(name)
		if !l.tableFilter.MatchTable(schema, table) {
			log.Info("filter tables", zap.String("schema", schema),
				zap.String("table", table), zap.Int64("tableID", tableID))
			continue
		}
		tableIDs = append(tableIDs, tableID)
	}

	for _, tID := range tableIDs {
		tableID := tID
		// FIXME update log meta logic here
		dir := fmt.Sprintf("%s%d", tableLogPrefix, tableID)
		opt := &storage.WalkOption{
			SubDir:    dir,
			ListCount: -1,
		}
		err := l.restoreClient.storage.WalkDir(ctx, opt, func(path string, size int64) error {
			fileName := filepath.Base(path)
			shouldRestore, err := l.NeedRestoreRowChange(fileName)
			if err != nil {
				return errors.Trace(err)
			}
			if shouldRestore {
				rowChangeFiles[tableID] = append(rowChangeFiles[tableID], path)
			}
			return nil
		})
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// sort file in order
	for tID, files := range rowChangeFiles {
		sortFiles := files
		sort.Slice(sortFiles, func(i, j int) bool {
			if filepath.Base(sortFiles[j]) == logPrefix {
				return true
			}
			return sortFiles[i] < sortFiles[j]
		})
		rowChangeFiles[tID] = sortFiles
	}

	return rowChangeFiles, nil
}

func (l *LogClient) writeRows(ctx context.Context, kvs kv.Pairs) error {
	log.Info("writeRows", zap.Int("kv count", len(kvs)))
	if len(kvs) == 0 {
		// shouldn't happen
		log.Warn("not rows to write")
		return nil
	}

	// stable sort kvs in memory
	sort.SliceStable(kvs, func(i, j int) bool {
		return bytes.Compare(kvs[i].Key, kvs[j].Key) < 0
	})

	// remove duplicate keys, and keep the last one
	newKvs := make([]kv.Pair, 0, len(kvs))
	for i := 0; i < len(kvs); i++ {
		if i == len(kvs)-1 {
			newKvs = append(newKvs, kvs[i])
			break
		}
		if bytes.Equal(kvs[i].Key, kvs[i+1].Key) {
			// skip this one
			continue
		}
		newKvs = append(newKvs, kvs[i])
	}

	remainRange := newSyncdRanges()
	remainRange.add(Range{
		Start: newKvs[0].Key,
		End:   kv.NextKey(newKvs[len(newKvs)-1].Key),
	})
	iterProducer := kv.NewSimpleKVIterProducer(newKvs)
	for {
		remain := remainRange.take()
		if len(remain) == 0 {
			log.Info("writeRows finish")
			break
		}
		eg, ectx := errgroup.WithContext(ctx)
		for _, r := range remain {
			rangeReplica := r
			l.ingester.WorkerPool.ApplyOnErrorGroup(eg, func() error {
				err := l.ingester.writeAndIngestByRange(ectx, iterProducer, rangeReplica.Start, rangeReplica.End, remainRange)
				if err != nil {
					log.Warn("writeRows failed with range", zap.Any("range", rangeReplica), zap.Error(err))
					return errors.Trace(err)
				}
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return errors.Trace(err)
		}
		log.Info("writeRows ranges unfinished, retry it", zap.Int("remain ranges", len(remain)))
	}
	return nil
}

func (l *LogClient) reloadTableMeta(dom *domain.Domain, tableID int64, item *cdclog.SortItem) error {
	err := dom.Reload()
	if err != nil {
		return errors.Trace(err)
	}
	// find tableID for this table on cluster
	newTableID := l.tableBuffers[tableID].TableID()
	var (
		newTableInfo titable.Table
		ok           bool
	)
	if newTableID != 0 {
		newTableInfo, ok = dom.InfoSchema().TableByID(newTableID)
		if !ok {
			log.Error("[restoreFromPuller] can't get table info from dom by tableID",
				zap.Int64("backup table id", tableID),
				zap.Int64("restore table id", newTableID),
			)
			return errors.Trace(err)
		}
	} else {
		// fall back to use schema table get info
		newTableInfo, err = dom.InfoSchema().TableByName(
			model.NewCIStr(item.Schema), model.NewCIStr(item.Table))
		if err != nil {
			log.Error("[restoreFromPuller] can't get table info from dom by table name",
				zap.Int64("backup table id", tableID),
				zap.Int64("restore table id", newTableID),
				zap.String("restore table name", item.Table),
				zap.String("restore schema name", item.Schema),
			)
			return errors.Trace(err)
		}
	}

	dbInfo, ok := dom.InfoSchema().SchemaByName(model.NewCIStr(item.Schema))
	if !ok {
		return errors.Annotatef(berrors.ErrRestoreSchemaNotExists, "schema %s", item.Schema)
	}
	allocs := autoid.NewAllocatorsFromTblInfo(dom.Store(), dbInfo.ID, newTableInfo.Meta())

	// reload
	l.tableBuffers[tableID].ReloadMeta(newTableInfo, allocs)
	log.Debug("reload table meta for table",
		zap.Int64("backup table id", tableID),
		zap.Int64("restore table id", newTableID),
		zap.String("restore table name", item.Table),
		zap.String("restore schema name", item.Schema),
		zap.Any("allocator", len(allocs)),
		zap.Any("auto", newTableInfo.Meta().GetAutoIncrementColInfo()),
	)
	return nil
}

func (l *LogClient) applyKVChanges(ctx context.Context, tableID int64) error {
	log.Info("apply kv changes to tikv",
		zap.Any("table", tableID),
	)
	dataKVs := kv.Pairs{}
	indexKVs := kv.Pairs{}

	tableBuffer := l.tableBuffers[tableID]
	if tableBuffer.IsEmpty() {
		log.Warn("no kv changes to apply")
		return nil
	}

	var dataChecksum, indexChecksum kv.Checksum
	for _, p := range tableBuffer.KvPairs {
		p.ClassifyAndAppend(&dataKVs, &dataChecksum, &indexKVs, &indexChecksum)
	}

	err := l.writeRows(ctx, dataKVs)
	if err != nil {
		return errors.Trace(err)
	}
	dataKVs = dataKVs.Clear()

	err = l.writeRows(ctx, indexKVs)
	if err != nil {
		return errors.Trace(err)
	}
	indexKVs = indexKVs.Clear()

	tableBuffer.Clear()

	return nil
}

func (l *LogClient) restoreTableFromPuller(
	ctx context.Context,
	tableID int64,
	puller *cdclog.EventPuller,
	dom *domain.Domain) error {
	for {
		item, err := puller.PullOneEvent(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		if item == nil {
			log.Info("[restoreFromPuller] nothing in this puller, we should stop and flush",
				zap.Int64("table id", tableID))
			err = l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}
			return nil
		}
		log.Debug("[restoreFromPuller] next event", zap.Any("item", item), zap.Int64("table id", tableID))
		if l.startTS > item.TS {
			log.Debug("[restoreFromPuller] item ts is smaller than start ts, skip this item",
				zap.Uint64("start ts", l.startTS),
				zap.Uint64("end ts", l.endTS),
				zap.Uint64("item ts", item.TS),
				zap.Int64("table id", tableID))
			continue
		}
		if l.endTS < item.TS {
			log.Warn("[restoreFromPuller] ts is larger than end ts, we should stop and flush",
				zap.Uint64("start ts", l.startTS),
				zap.Uint64("end ts", l.endTS),
				zap.Uint64("item ts", item.TS),
				zap.Int64("table id", tableID))
			err = l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}
			return nil
		}

		if l.shouldFilter(item) {
			log.Debug("[restoreFromPuller] filter item because later drop schema will affect on this item",
				zap.Any("item", item),
				zap.Int64("table id", tableID))
			err = l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}

		switch item.ItemType {
		case cdclog.DDL:
			name := l.meta.Names[tableID]
			schema, table := ParseQuoteName(name)
			ddl := item.Data.(*cdclog.MessageDDL)
			// ddl not influence on this schema/table
			if !(schema == item.Schema && (table == item.Table || l.isDBRelatedDDL(ddl))) {
				log.Info("[restoreFromPuller] meet unrelated ddl, and continue pulling",
					zap.String("item table", item.Table),
					zap.String("table", table),
					zap.String("item schema", item.Schema),
					zap.String("schema", schema),
					zap.Int64("backup table id", tableID),
					zap.String("query", ddl.Query),
					zap.Int64("table id", tableID))
				continue
			}

			// database level ddl job has been executed at the beginning
			if l.isDBRelatedDDL(ddl) {
				log.Debug("[restoreFromPuller] meet database level ddl, continue pulling",
					zap.String("ddl", ddl.Query),
					zap.Int64("table id", tableID))
				continue
			}

			// wait all previous kvs ingest finished
			err = l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}

			log.Debug("[restoreFromPuller] execute ddl", zap.String("ddl", ddl.Query))

			l.ddlLock.Lock()
			err = l.restoreClient.db.se.Execute(ctx, fmt.Sprintf("use %s", item.Schema))
			if err != nil {
				return errors.Trace(err)
			}

			err = l.restoreClient.db.se.Execute(ctx, ddl.Query)
			if err != nil {
				return errors.Trace(err)
			}
			l.ddlLock.Unlock()

			// if table dropped, we will pull next event to see if this table will create again.
			// with next create table ddl, we can do reloadTableMeta.
			if l.isDropTable(ddl) {
				log.Info("[restoreFromPuller] skip reload because this is a drop table ddl",
					zap.String("ddl", ddl.Query))
				l.tableBuffers[tableID].ResetTableInfo()
				continue
			}
			err = l.reloadTableMeta(dom, tableID, item)
			if err != nil {
				return errors.Trace(err)
			}
		case cdclog.RowChanged:
			if l.tableBuffers[tableID].TableInfo() == nil {
				err = l.reloadTableMeta(dom, tableID, item)
				if err != nil {
					// shouldn't happen
					return errors.Trace(err)
				}
			}
			err = l.tableBuffers[tableID].Append(item)
			if err != nil {
				return errors.Trace(err)
			}
			if l.tableBuffers[tableID].ShouldApply() {
				err = l.applyKVChanges(ctx, tableID)
				if err != nil {
					return errors.Trace(err)
				}
			}
		}
	}
}

func (l *LogClient) restoreTables(ctx context.Context, dom *domain.Domain) error {
	// 1. decode cdclog with in ts range
	// 2. dispatch cdclog events to table level concurrently
	// 		a. encode row changed files to kvpairs and ingest into tikv
	// 		b. exec ddl
	log.Debug("start restore tables")
	workerPool := utils.NewWorkerPool(l.concurrencyCfg.Concurrency, "table log restore")
	eg, ectx := errgroup.WithContext(ctx)
	for tableID, puller := range l.eventPullers {
		pullerReplica := puller
		tableIDReplica := tableID
		workerPool.ApplyOnErrorGroup(eg, func() error {
			return l.restoreTableFromPuller(ectx, tableIDReplica, pullerReplica, dom)
		})
	}
	return eg.Wait()
}

// RestoreLogData restore specify log data from storage.
func (l *LogClient) RestoreLogData(ctx context.Context, dom *domain.Domain) error {
	// 1. Retrieve log data from storage
	// 2. Find proper data by TS range
	// 3. Encode and ingest data to tikv

	// parse meta file
	data, err := l.restoreClient.storage.ReadFile(ctx, metaFile)
	if err != nil {
		return errors.Trace(err)
	}
	err = json.Unmarshal(data, l.meta)
	if err != nil {
		return errors.Trace(err)
	}
	log.Info("get meta from storage", zap.Binary("data", data))

	if l.startTS > l.meta.GlobalResolvedTS {
		return errors.Annotatef(berrors.ErrRestoreRTsConstrain,
			"start ts:%d is greater than resolved ts:%d", l.startTS, l.meta.GlobalResolvedTS)
	}
	if l.endTS > l.meta.GlobalResolvedTS {
		log.Info("end ts is greater than resolved ts,"+
			" to keep consistency we only recover data until resolved ts",
			zap.Uint64("end ts", l.endTS),
			zap.Uint64("resolved ts", l.meta.GlobalResolvedTS))
		l.endTS = l.meta.GlobalResolvedTS
	}

	// collect ddl files
	ddlFiles, err := l.collectDDLFiles(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	log.Info("collect ddl files", zap.Any("files", ddlFiles))

	err = l.doDBDDLJob(ctx, ddlFiles)
	if err != nil {
		return errors.Trace(err)
	}
	log.Debug("db level ddl executed")

	// collect row change files
	rowChangesFiles, err := l.collectRowChangeFiles(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	log.Info("collect row changed files", zap.Any("files", rowChangesFiles))

	// create event puller to apply changes concurrently
	for tableID, files := range rowChangesFiles {
		name := l.meta.Names[tableID]
		schema, table := ParseQuoteName(name)
		log.Info("create puller for table",
			zap.Int64("table id", tableID),
			zap.String("schema", schema),
			zap.String("table", table),
		)
		l.eventPullers[tableID], err = cdclog.NewEventPuller(ctx, schema, table, ddlFiles, files, l.restoreClient.storage)
		if err != nil {
			return errors.Trace(err)
		}
		// use table name to get table info
		var tableInfo titable.Table
		var allocs autoid.Allocators
		infoSchema := dom.InfoSchema()
		if infoSchema.TableExists(model.NewCIStr(schema), model.NewCIStr(table)) {
			tableInfo, err = infoSchema.TableByName(model.NewCIStr(schema), model.NewCIStr(table))
			if err != nil {
				return errors.Trace(err)
			}
			dbInfo, ok := dom.InfoSchema().SchemaByName(model.NewCIStr(schema))
			if !ok {
				return errors.Annotatef(berrors.ErrRestoreSchemaNotExists, "schema %s", schema)
			}
			allocs = autoid.NewAllocatorsFromTblInfo(dom.Store(), dbInfo.ID, tableInfo.Meta())
		}

		l.tableBuffers[tableID] = cdclog.NewTableBuffer(tableInfo, allocs,
			l.concurrencyCfg.BatchFlushKVPairs, l.concurrencyCfg.BatchFlushKVSize)
	}
	// restore files
	return l.restoreTables(ctx, dom)
}
