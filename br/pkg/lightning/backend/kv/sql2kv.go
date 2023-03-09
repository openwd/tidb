// Copyright 2019 PingCAP, Inc.
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

// TODO combine with the pkg/kv package outside.

package kv

import (
	"context"
	"fmt"
	"math"
	"math/rand"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/lightning/common"
	"github.com/pingcap/tidb/br/pkg/lightning/log"
	"github.com/pingcap/tidb/br/pkg/lightning/metric"
	"github.com/pingcap/tidb/br/pkg/lightning/verification"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/br/pkg/redact"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql" //nolint: goimports
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/slices"
)

var ExtraHandleColumnInfo = model.NewExtraHandleColInfo()

type genCol struct {
	index int
	expr  expression.Expression
}

type autoIDConverter func(int64) int64

type tableKVEncoder struct {
	tbl             table.Table
	autoRandomColID int64
	se              *session
	recordCache     []types.Datum
	genCols         []genCol
	// convert auto id for shard rowid or auto random id base on row id generated by lightning
	autoIDFn autoIDConverter
	metrics  *metric.Metrics
}

func GetSession4test(encoder Encoder) sessionctx.Context {
	return encoder.(*tableKVEncoder).se
}

func NewTableKVEncoder(
	tbl table.Table,
	options *SessionOptions,
	metrics *metric.Metrics,
	logger log.Logger,
) (Encoder, error) {
	if metrics != nil {
		metrics.KvEncoderCounter.WithLabelValues("open").Inc()
	}
	meta := tbl.Meta()
	cols := tbl.Cols()
	se := newSession(options, logger)
	// Set CommonAddRecordCtx to session to reuse the slices and BufStore in AddRecord
	recordCtx := tables.NewCommonAddRecordCtx(len(cols))
	tables.SetAddRecordCtx(se, recordCtx)

	var autoRandomColID int64
	autoIDFn := func(id int64) int64 { return id }
	if meta.ContainsAutoRandomBits() {
		col := common.GetAutoRandomColumn(meta)
		autoRandomColID = col.ID

		shardFmt := autoid.NewShardIDFormat(&col.FieldType, meta.AutoRandomBits, meta.AutoRandomRangeBits)
		shard := rand.New(rand.NewSource(options.AutoRandomSeed)).Int63()
		autoIDFn = func(id int64) int64 {
			return shardFmt.Compose(shard, id)
		}
	} else if meta.ShardRowIDBits > 0 {
		rd := rand.New(rand.NewSource(options.AutoRandomSeed)) // nolint:gosec
		mask := int64(1)<<meta.ShardRowIDBits - 1
		shift := autoid.RowIDBitLength - meta.ShardRowIDBits - 1
		autoIDFn = func(id int64) int64 {
			rd.Seed(id)
			shardBits := (int64(rd.Uint32()) & mask) << shift
			return shardBits | id
		}
	}

	// collect expressions for evaluating stored generated columns
	genCols, err := collectGeneratedColumns(se, meta, cols)
	if err != nil {
		return nil, errors.Annotate(err, "failed to parse generated column expressions")
	}

	return &tableKVEncoder{
		tbl:             tbl,
		autoRandomColID: autoRandomColID,
		se:              se,
		genCols:         genCols,
		autoIDFn:        autoIDFn,
		metrics:         metrics,
	}, nil
}

// collectGeneratedColumns collects all expressions required to evaluate the
// results of all generated columns. The returning slice is in evaluation order.
func collectGeneratedColumns(se *session, meta *model.TableInfo, cols []*table.Column) ([]genCol, error) {
	hasGenCol := false
	for _, col := range cols {
		if col.GeneratedExpr != nil {
			hasGenCol = true
			break
		}
	}

	if !hasGenCol {
		return nil, nil
	}

	// the expression rewriter requires a non-nil TxnCtx.
	se.vars.TxnCtx = new(variable.TransactionContext)
	defer func() {
		se.vars.TxnCtx = nil
	}()

	// not using TableInfo2SchemaAndNames to avoid parsing all virtual generated columns again.
	exprColumns := make([]*expression.Column, 0, len(cols))
	names := make(types.NameSlice, 0, len(cols))
	for i, col := range cols {
		names = append(names, &types.FieldName{
			OrigTblName: meta.Name,
			OrigColName: col.Name,
			TblName:     meta.Name,
			ColName:     col.Name,
		})
		exprColumns = append(exprColumns, &expression.Column{
			RetType:  col.FieldType.Clone(),
			ID:       col.ID,
			UniqueID: int64(i),
			Index:    col.Offset,
			OrigName: names[i].String(),
			IsHidden: col.Hidden,
		})
	}
	schema := expression.NewSchema(exprColumns...)

	// as long as we have a stored generated column, all columns it referred to must be evaluated as well.
	// for simplicity we just evaluate all generated columns (virtual or not) before the last stored one.
	var genCols []genCol
	for i, col := range cols {
		if col.GeneratedExpr != nil {
			expr, err := expression.RewriteAstExpr(se, col.GeneratedExpr, schema, names, true)
			if err != nil {
				return nil, err
			}
			genCols = append(genCols, genCol{
				index: i,
				expr:  expr,
			})
		}
	}

	// order the result by column offset so they match the evaluation order.
	slices.SortFunc(genCols, func(i, j genCol) bool {
		return cols[i.index].Offset < cols[j.index].Offset
	})
	return genCols, nil
}

func (kvcodec *tableKVEncoder) Close() {
	kvcodec.se.Close()
	if kvcodec.metrics != nil {
		kvcodec.metrics.KvEncoderCounter.WithLabelValues("close").Inc()
	}
}

// RowArrayMarshaler wraps a slice of types.Datum for logging the content into zap.
type RowArrayMarshaler []types.Datum

var kindStr = [...]string{
	types.KindNull:          "null",
	types.KindInt64:         "int64",
	types.KindUint64:        "uint64",
	types.KindFloat32:       "float32",
	types.KindFloat64:       "float64",
	types.KindString:        "string",
	types.KindBytes:         "bytes",
	types.KindBinaryLiteral: "binary",
	types.KindMysqlDecimal:  "decimal",
	types.KindMysqlDuration: "duration",
	types.KindMysqlEnum:     "enum",
	types.KindMysqlBit:      "bit",
	types.KindMysqlSet:      "set",
	types.KindMysqlTime:     "time",
	types.KindInterface:     "interface",
	types.KindMinNotNull:    "min",
	types.KindMaxValue:      "max",
	types.KindRaw:           "raw",
	types.KindMysqlJSON:     "json",
}

// MarshalLogArray implements the zapcore.ArrayMarshaler interface
func (row RowArrayMarshaler) MarshalLogArray(encoder zapcore.ArrayEncoder) error {
	for _, datum := range row {
		kind := datum.Kind()
		var str string
		var err error
		switch kind {
		case types.KindNull:
			str = "NULL"
		case types.KindMinNotNull:
			str = "-inf"
		case types.KindMaxValue:
			str = "+inf"
		default:
			str, err = datum.ToString()
			if err != nil {
				return err
			}
		}
		if err := encoder.AppendObject(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
			enc.AddString("kind", kindStr[kind])
			enc.AddString("val", redact.String(str))
			return nil
		})); err != nil {
			return err
		}
	}
	return nil
}

func logKVConvertFailed(logger log.Logger, row []types.Datum, j int, colInfo *model.ColumnInfo, err error) error {
	var original types.Datum
	if 0 <= j && j < len(row) {
		original = row[j]
		row = row[j : j+1]
	}

	logger.Error("kv convert failed",
		zap.Array("original", RowArrayMarshaler(row)),
		zap.Int("originalCol", j),
		zap.String("colName", colInfo.Name.O),
		zap.Stringer("colType", &colInfo.FieldType),
		log.ShortError(err),
	)

	logger.Error("failed to convert kv value", logutil.RedactAny("origVal", original.GetValue()),
		zap.Stringer("fieldType", &colInfo.FieldType), zap.String("column", colInfo.Name.O),
		zap.Int("columnID", j+1))
	return errors.Annotatef(
		err,
		"failed to cast value as %s for column `%s` (#%d)", &colInfo.FieldType, colInfo.Name.O, j+1,
	)
}

func logEvalGenExprFailed(logger log.Logger, row []types.Datum, colInfo *model.ColumnInfo, err error) error {
	logger.Error("kv convert failed: cannot evaluate generated column expression",
		zap.Array("original", RowArrayMarshaler(row)),
		zap.String("colName", colInfo.Name.O),
		log.ShortError(err),
	)

	return errors.Annotatef(
		err,
		"failed to evaluate generated column expression for column `%s`",
		colInfo.Name.O,
	)
}

type KvPairs struct {
	pairs    []common.KvPair
	bytesBuf *bytesBuf
	memBuf   *kvMemBuf
}

// MakeRowsFromKvPairs converts a KvPair slice into a Rows instance. This is
// mainly used for testing only. The resulting Rows instance should only be used
// for the importer backend.
func MakeRowsFromKvPairs(pairs []common.KvPair) Rows {
	return &KvPairs{pairs: pairs}
}

// MakeRowFromKvPairs converts a KvPair slice into a Row instance. This is
// mainly used for testing only. The resulting Row instance should only be used
// for the importer backend.
func MakeRowFromKvPairs(pairs []common.KvPair) Row {
	return &KvPairs{pairs: pairs}
}

// KvPairsFromRows converts a Rows instance constructed from MakeRowsFromKvPairs
// back into a slice of KvPair. This method panics if the Rows is not
// constructed in such way.
// nolint:golint // kv.KvPairsFromRows sounds good.
func KvPairsFromRows(rows Rows) []common.KvPair {
	return rows.(*KvPairs).pairs
}

// KvPairsFromRow converts a Row instance constructed from MakeRowFromKvPairs
// back into a slice of KvPair. This method panics if the Row is not
// constructed in such way.
// nolint:golint // kv.KvPairsFromRow sounds good.
func KvPairsFromRow(row Row) []common.KvPair {
	return row.(*KvPairs).pairs
}

func evaluateGeneratedColumns(se *session, record []types.Datum, cols []*table.Column, genCols []genCol) (errCol *model.ColumnInfo, err error) {
	mutRow := chunk.MutRowFromDatums(record)
	for _, gc := range genCols {
		col := cols[gc.index].ToInfo()
		evaluated, err := gc.expr.Eval(mutRow.ToRow())
		if err != nil {
			return col, err
		}
		value, err := table.CastValue(se, evaluated, col, false, false)
		if err != nil {
			return col, err
		}
		mutRow.SetDatum(gc.index, value)
		record[gc.index] = value
	}
	return nil, nil
}

// Encode a row of data into KV pairs.
//
// See comments in `(*TableRestore).initializeColumns` for the meaning of the
// `columnPermutation` parameter.
func (kvcodec *tableKVEncoder) Encode(
	logger log.Logger,
	row []types.Datum,
	rowID int64,
	columnPermutation []int,
	_ string,
	offset int64,
) (Row, error) {
	cols := kvcodec.tbl.Cols()

	var value types.Datum
	var err error
	//nolint: prealloc
	var record []types.Datum

	if kvcodec.recordCache != nil {
		record = kvcodec.recordCache
	} else {
		record = make([]types.Datum, 0, len(cols)+1)
	}

	meta := kvcodec.tbl.Meta()
	for i, col := range cols {
		var theDatum *types.Datum = nil
		j := columnPermutation[i]
		if j >= 0 && j < len(row) {
			theDatum = &row[j]
		}
		value, err = kvcodec.getActualDatum(rowID, i, theDatum)
		if err != nil {
			return nil, logKVConvertFailed(logger, row, j, col.ToInfo(), err)
		}

		record = append(record, value)

		if kvcodec.isAutoRandomCol(col.ToInfo()) {
			shardFmt := autoid.NewShardIDFormat(&col.FieldType, meta.AutoRandomBits, meta.AutoRandomRangeBits)
			// this allocator is the same as the allocator in table importer, i.e. PanickingAllocators. below too.
			alloc := kvcodec.tbl.Allocators(kvcodec.se).Get(autoid.AutoRandomType)
			if err := alloc.Rebase(context.Background(), value.GetInt64()&shardFmt.IncrementalMask(), false); err != nil {
				return nil, errors.Trace(err)
			}
		}
		if isAutoIncCol(col.ToInfo()) {
			alloc := kvcodec.tbl.Allocators(kvcodec.se).Get(autoid.AutoIncrementType)
			if err := alloc.Rebase(context.Background(), getAutoRecordID(value, &col.FieldType), false); err != nil {
				return nil, errors.Trace(err)
			}
		}
	}

	if common.TableHasAutoRowID(meta) {
		rowValue := rowID
		j := columnPermutation[len(cols)]
		if j >= 0 && j < len(row) {
			value, err = table.CastValue(kvcodec.se, row[j], ExtraHandleColumnInfo, false, false)
			rowValue = value.GetInt64()
		} else {
			rowID := kvcodec.autoIDFn(rowID)
			value, err = types.NewIntDatum(rowID), nil
		}
		if err != nil {
			return nil, logKVConvertFailed(logger, row, j, ExtraHandleColumnInfo, err)
		}
		record = append(record, value)
		alloc := kvcodec.tbl.Allocators(kvcodec.se).Get(autoid.RowIDAllocType)
		if err := alloc.Rebase(context.Background(), rowValue, false); err != nil {
			return nil, errors.Trace(err)
		}
	}

	if len(kvcodec.genCols) > 0 {
		if errCol, err := evaluateGeneratedColumns(kvcodec.se, record, cols, kvcodec.genCols); err != nil {
			return nil, logEvalGenExprFailed(logger, row, errCol, err)
		}
	}

	_, err = kvcodec.tbl.AddRecord(kvcodec.se, record)
	if err != nil {
		logger.Error("kv encode failed",
			zap.Array("originalRow", RowArrayMarshaler(row)),
			zap.Array("convertedRow", RowArrayMarshaler(record)),
			log.ShortError(err),
		)
		return nil, errors.Trace(err)
	}
	kvPairs := kvcodec.se.takeKvPairs()
	for i := 0; i < len(kvPairs.pairs); i++ {
		var encoded [9]byte // The max length of encoded int64 is 9.
		kvPairs.pairs[i].RowID = common.EncodeIntRowIDToBuf(encoded[:0], rowID)
	}
	kvcodec.recordCache = record[:0]
	return kvPairs, nil
}

func (kvcodec *tableKVEncoder) isAutoRandomCol(col *model.ColumnInfo) bool {
	return kvcodec.tbl.Meta().ContainsAutoRandomBits() && col.ID == kvcodec.autoRandomColID
}

func isAutoIncCol(colInfo *model.ColumnInfo) bool {
	return mysql.HasAutoIncrementFlag(colInfo.GetFlag())
}

// GetEncoderIncrementalID return Auto increment id.
func GetEncoderIncrementalID(encoder Encoder, id int64) int64 {
	return encoder.(*tableKVEncoder).autoIDFn(id)
}

// GetEncoderSe return session.
func GetEncoderSe(encoder Encoder) *session {
	return encoder.(*tableKVEncoder).se
}

// GetActualDatum export getActualDatum function.
func GetActualDatum(encoder Encoder, rowID int64, colIndex int, inputDatum *types.Datum) (types.Datum, error) {
	return encoder.(*tableKVEncoder).getActualDatum(70, 0, inputDatum)
}

func (kvcodec *tableKVEncoder) getActualDatum(rowID int64, colIndex int, inputDatum *types.Datum) (types.Datum, error) {
	var (
		value types.Datum
		err   error
	)

	cols := kvcodec.tbl.Cols()

	// Since this method is only called when iterating the columns in the `Encode()` method,
	// we can assume that the `colIndex` always have a valid input
	col := cols[colIndex]

	isBadNullValue := false
	if inputDatum != nil {
		value, err = table.CastValue(kvcodec.se, *inputDatum, col.ToInfo(), false, false)
		if err != nil {
			return value, err
		}
		if err := col.CheckNotNull(&value, 0); err == nil {
			return value, nil // the most normal case
		}
		isBadNullValue = true
	}
	// handle special values
	switch {
	case isAutoIncCol(col.ToInfo()):
		// we still need a conversion, e.g. to catch overflow with a TINYINT column.
		value, err = table.CastValue(kvcodec.se, types.NewIntDatum(rowID), col.ToInfo(), false, false)
	case kvcodec.isAutoRandomCol(col.ToInfo()):
		var val types.Datum
		realRowID := kvcodec.autoIDFn(rowID)
		if mysql.HasUnsignedFlag(col.GetFlag()) {
			val = types.NewUintDatum(uint64(realRowID))
		} else {
			val = types.NewIntDatum(realRowID)
		}
		value, err = table.CastValue(kvcodec.se, val, col.ToInfo(), false, false)
	case col.IsGenerated():
		// inject some dummy value for gen col so that MutRowFromDatums below sees a real value instead of nil.
		// if MutRowFromDatums sees a nil it won't initialize the underlying storage and cause SetDatum to panic.
		value = types.GetMinValue(&col.FieldType)
	case isBadNullValue:
		err = col.HandleBadNull(&value, kvcodec.se.vars.StmtCtx, 0)
	default:
		value, err = table.GetColDefaultValue(kvcodec.se, col.ToInfo())
	}
	return value, err
}

// get record value for auto-increment field
//
// See: https://github.com/pingcap/tidb/blob/47f0f15b14ed54fc2222f3e304e29df7b05e6805/executor/insert_common.go#L781-L852
func getAutoRecordID(d types.Datum, target *types.FieldType) int64 {
	switch target.GetType() {
	case mysql.TypeFloat, mysql.TypeDouble:
		return int64(math.Round(d.GetFloat64()))
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong:
		return d.GetInt64()
	default:
		panic(fmt.Sprintf("unsupported auto-increment field type '%d'", target.GetType()))
	}
}

func (kvs *KvPairs) Size() uint64 {
	size := uint64(0)
	for _, kv := range kvs.pairs {
		size += uint64(len(kv.Key) + len(kv.Val))
	}
	return size
}

func (kvs *KvPairs) ClassifyAndAppend(
	data *Rows,
	dataChecksum *verification.KVChecksum,
	indices *Rows,
	indexChecksum *verification.KVChecksum,
) {
	dataKVs := (*data).(*KvPairs)
	indexKVs := (*indices).(*KvPairs)

	for _, kv := range kvs.pairs {
		if kv.Key[tablecodec.TableSplitKeyLen+1] == 'r' {
			dataKVs.pairs = append(dataKVs.pairs, kv)
			dataChecksum.UpdateOne(kv)
		} else {
			indexKVs.pairs = append(indexKVs.pairs, kv)
			indexChecksum.UpdateOne(kv)
		}
	}

	// the related buf is shared, so we only need to set it into one of the kvs so it can be released
	if kvs.bytesBuf != nil {
		dataKVs.bytesBuf = kvs.bytesBuf
		dataKVs.memBuf = kvs.memBuf
		kvs.bytesBuf = nil
		kvs.memBuf = nil
	}

	*data = dataKVs
	*indices = indexKVs
}

func (kvs *KvPairs) SplitIntoChunks(splitSize int) []Rows {
	if len(kvs.pairs) == 0 {
		return nil
	}

	res := make([]Rows, 0, 1)
	i := 0
	cumSize := 0
	for j, pair := range kvs.pairs {
		size := len(pair.Key) + len(pair.Val)
		if i < j && cumSize+size > splitSize {
			res = append(res, &KvPairs{pairs: kvs.pairs[i:j]})
			i = j
			cumSize = 0
		}
		cumSize += size
	}

	if i == 0 {
		res = append(res, kvs)
	} else {
		res = append(res, &KvPairs{
			pairs:    kvs.pairs[i:],
			bytesBuf: kvs.bytesBuf,
			memBuf:   kvs.memBuf,
		})
	}
	return res
}

func (kvs *KvPairs) Clear() Rows {
	if kvs.bytesBuf != nil {
		kvs.memBuf.Recycle(kvs.bytesBuf)
		kvs.bytesBuf = nil
		kvs.memBuf = nil
	}
	kvs.pairs = kvs.pairs[:0]
	return kvs
}
