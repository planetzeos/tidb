// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"sync"
	"time"

	"github.com/pingcap/tidb/executor/aggfuncs"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/set"
	"github.com/pkg/errors"
	"github.com/spaolacci/murmur3"
	"golang.org/x/net/context"
)

type aggPartialResultMapper map[string][]aggfuncs.PartialResult

// baseHashAggWorker stores the common attributes of HashAggFinalWorker and HashAggPartialWorker.
type baseHashAggWorker struct {
	finishCh     <-chan struct{}
	aggFuncs     []aggfuncs.AggFunc
	maxChunkSize int
}

func newBaseHashAggWorker(finishCh <-chan struct{}, aggFuncs []aggfuncs.AggFunc, maxChunkSize int) baseHashAggWorker {
	return baseHashAggWorker{
		finishCh:     finishCh,
		aggFuncs:     aggFuncs,
		maxChunkSize: maxChunkSize,
	}
}

// HashAggPartialWorker indicates the partial workers of parallel hash agg execution,
// the number of the worker can be set by `tidb_hashagg_partial_concurrency`.
type HashAggPartialWorker struct {
	baseHashAggWorker

	inputCh           chan *chunk.Chunk
	outputChs         []chan *HashAggIntermData
	globalOutputCh    chan *AfFinalResult
	giveBackCh        chan<- *HashAggInput
	partialResultsMap aggPartialResultMapper
	groupByItems      []expression.Expression
	groupKey          []byte
	groupValDatums    []types.Datum
	// chk stores the input data from child,
	// and is reused by childExec and partial worker.
	chk *chunk.Chunk
}

// HashAggFinalWorker indicates the final workers of parallel hash agg execution,
// the number of the worker can be set by `tidb_hashagg_final_concurrency`.
type HashAggFinalWorker struct {
	baseHashAggWorker

	rowBuffer           []types.Datum
	mutableRow          chunk.MutRow
	partialResultMap    aggPartialResultMapper
	groupSet            set.StringSet
	inputCh             chan *HashAggIntermData
	outputCh            chan *AfFinalResult
	finalResultHolderCh chan *chunk.Chunk
}

// AfFinalResult indicates aggregation functions final result.
type AfFinalResult struct {
	chk *chunk.Chunk
	err error

	giveBackCh chan *chunk.Chunk
}

// HashAggExec deals with all the aggregate functions.
// It is built from the Aggregate Plan. When Next() is called, it reads all the data from Src
// and updates all the items in PartialAggFuncs.
// The parallel execution flow is as the following graph shows:
//
//                            +-------------+
//                            | Main Thread |
//                            +------+------+
//                                   ^
//                                   |
//                                   +
//                              +-+-            +-+
//                              | |    ......   | |  finalOutputCh
//                              +++-            +-+
//                               ^
//                               |
//                               +---------------+
//                               |               |
//                 +--------------+             +--------------+
//                 | final worker |     ......  | final worker |
//                 +------------+-+             +-+------------+
//                              ^                 ^
//                              |                 |
//                             +-+  +-+  ......  +-+
//                             | |  | |          | |
//                             ...  ...          ...    partialOutputChs
//                             | |  | |          | |
//                             +++  +++          +++
//                              ^    ^            ^
//          +-+                 |    |            |
//          | |        +--------o----+            |
// inputCh  +-+        |        +-----------------+---+
//          | |        |                              |
//          ...    +---+------------+            +----+-----------+
//          | |    | partial worker |   ......   | partial worker |
//          +++    +--------------+-+            +-+--------------+
//           |                     ^                ^
//           |                     |                |
//      +----v---------+          +++ +-+          +++
//      | data fetcher | +------> | | | |  ......  | |   partialInputChs
//      +--------------+          +-+ +-+          +-+
type HashAggExec struct {
	baseExecutor

	prepared         bool
	sc               *stmtctx.StatementContext
	PartialAggFuncs  []aggfuncs.AggFunc
	FinalAggFuncs    []aggfuncs.AggFunc
	partialResultMap aggPartialResultMapper
	groupSet         set.StringSet
	groupKeys        []string
	cursor4GroupKey  int
	GroupByItems     []expression.Expression
	groupKeyBuffer   []byte
	groupValDatums   []types.Datum

	// After we support parallel execution for aggregation functions with distinct,
	// we can remove this attribute.
	isUnparallelExec bool

	finishCh         chan struct{}
	finalOutputCh    chan *AfFinalResult
	partialOutputChs []chan *HashAggIntermData
	inputCh          chan *HashAggInput
	partialInputChs  []chan *chunk.Chunk
	partialWorkers   []HashAggPartialWorker
	finalWorkers     []HashAggFinalWorker
	defaultVal       *chunk.Chunk
	// isChildReturnEmpty indicates whether the child executor only returns an empty input.
	isChildReturnEmpty bool

	childResult *chunk.Chunk
}

// HashAggInput indicates the input of hash agg exec.
type HashAggInput struct {
	chk *chunk.Chunk
	// giveBackCh is bound with specific partial worker,
	// it's used to reuse the `chk`,
	// and tell the data-fetcher which partial worker it should send data to.
	giveBackCh chan<- *chunk.Chunk
}

// HashAggIntermData indicates the intermediate data of aggregation execution.
type HashAggIntermData struct {
	groupKeys        []string
	cursor           int
	partialResultMap aggPartialResultMapper
}

// getPartialResultBatch fetches a batch of partial results from HashAggIntermData.
func (d *HashAggIntermData) getPartialResultBatch(sc *stmtctx.StatementContext, prs [][]aggfuncs.PartialResult, aggFuncs []aggfuncs.AggFunc, maxChunkSize int) (_ [][]aggfuncs.PartialResult, groupKeys []string, reachEnd bool) {
	keyStart := d.cursor
	for ; d.cursor < len(d.groupKeys) && len(prs) < maxChunkSize; d.cursor++ {
		prs = append(prs, d.partialResultMap[d.groupKeys[d.cursor]])
	}
	if d.cursor == len(d.groupKeys) {
		reachEnd = true
	}
	return prs, d.groupKeys[keyStart:d.cursor], reachEnd
}

// Close implements the Executor Close interface.
func (e *HashAggExec) Close() error {
	if e.isUnparallelExec {
		e.childResult = nil
		e.groupSet = nil
		e.partialResultMap = nil
		return nil
	}
	// `Close` may be called after `Open` without calling `Next` in test.
	if !e.prepared {
		close(e.inputCh)
		for _, ch := range e.partialOutputChs {
			close(ch)
		}
		close(e.finalOutputCh)
	}
	close(e.finishCh)
	for _, ch := range e.partialOutputChs {
		for range ch {
		}
	}
	for range e.finalOutputCh {
	}
	return errors.Trace(e.baseExecutor.Close())
}

// Open implements the Executor Open interface.
func (e *HashAggExec) Open(ctx context.Context) error {
	if err := e.baseExecutor.Open(ctx); err != nil {
		return errors.Trace(err)
	}
	e.prepared = false

	if e.isUnparallelExec {
		e.initForUnparallelExec()
		return nil
	}
	e.initForParallelExec(e.ctx)
	return nil
}

func (e *HashAggExec) initForUnparallelExec() {
	e.groupSet = set.NewStringSet()
	e.partialResultMap = make(aggPartialResultMapper, 0)
	e.groupKeyBuffer = make([]byte, 0, 8)
	e.groupValDatums = make([]types.Datum, 0, len(e.groupKeyBuffer))
	e.childResult = e.children[0].newFirstChunk()
}

func (e *HashAggExec) initForParallelExec(ctx sessionctx.Context) {
	sessionVars := e.ctx.GetSessionVars()
	finalConcurrency := sessionVars.HashAggFinalConcurrency
	partialConcurrency := sessionVars.HashAggPartialConcurrency
	e.isChildReturnEmpty = true
	e.finalOutputCh = make(chan *AfFinalResult, finalConcurrency)
	e.inputCh = make(chan *HashAggInput, partialConcurrency)
	e.finishCh = make(chan struct{}, 1)

	e.partialInputChs = make([]chan *chunk.Chunk, partialConcurrency)
	for i := range e.partialInputChs {
		e.partialInputChs[i] = make(chan *chunk.Chunk, 1)
	}
	e.partialOutputChs = make([]chan *HashAggIntermData, finalConcurrency)
	for i := range e.partialOutputChs {
		e.partialOutputChs[i] = make(chan *HashAggIntermData, partialConcurrency)
	}

	e.partialWorkers = make([]HashAggPartialWorker, partialConcurrency)
	e.finalWorkers = make([]HashAggFinalWorker, finalConcurrency)

	// Init partial workers.
	for i := 0; i < partialConcurrency; i++ {
		w := HashAggPartialWorker{
			baseHashAggWorker: newBaseHashAggWorker(e.finishCh, e.PartialAggFuncs, e.maxChunkSize),
			inputCh:           e.partialInputChs[i],
			outputChs:         e.partialOutputChs,
			giveBackCh:        e.inputCh,
			globalOutputCh:    e.finalOutputCh,
			partialResultsMap: make(aggPartialResultMapper, 0),
			groupByItems:      e.GroupByItems,
			groupValDatums:    make([]types.Datum, 0, len(e.GroupByItems)),
			chk:               e.children[0].newFirstChunk(),
		}

		e.partialWorkers[i] = w
		e.inputCh <- &HashAggInput{
			chk:        e.children[0].newFirstChunk(),
			giveBackCh: w.inputCh,
		}
	}

	// Init final workers.
	for i := 0; i < finalConcurrency; i++ {
		e.finalWorkers[i] = HashAggFinalWorker{
			baseHashAggWorker:   newBaseHashAggWorker(e.finishCh, e.FinalAggFuncs, e.maxChunkSize),
			partialResultMap:    make(aggPartialResultMapper, 0),
			groupSet:            set.NewStringSet(),
			inputCh:             e.partialOutputChs[i],
			outputCh:            e.finalOutputCh,
			finalResultHolderCh: make(chan *chunk.Chunk, 1),
			rowBuffer:           make([]types.Datum, 0, e.Schema().Len()),
			mutableRow:          chunk.MutRowFromTypes(e.retTypes()),
		}
		e.finalWorkers[i].finalResultHolderCh <- e.newFirstChunk()
	}
}

func (w *HashAggPartialWorker) getChildInput() bool {
	select {
	case <-w.finishCh:
		return false
	case chk, ok := <-w.inputCh:
		if !ok {
			return false
		}
		w.chk.SwapColumns(chk)
		w.giveBackCh <- &HashAggInput{
			chk:        chk,
			giveBackCh: w.inputCh,
		}
	}
	return true
}

func (w *HashAggPartialWorker) run(ctx sessionctx.Context, waitGroup *sync.WaitGroup, finalConcurrency int) {
	needShuffle, sc := false, ctx.GetSessionVars().StmtCtx
	defer func() {
		if needShuffle {
			w.shuffleIntermData(sc, finalConcurrency)
		}
		waitGroup.Done()
	}()
	for {
		if !w.getChildInput() {
			return
		}
		if err := w.updatePartialResult(ctx, sc, w.chk, len(w.partialResultsMap)); err != nil {
			w.globalOutputCh <- &AfFinalResult{err: errors.Trace(err)}
			return
		}
		// The intermData can be promised to be not empty if reaching here,
		// so we set needShuffle to be true.
		needShuffle = true
	}
}

func (w *HashAggPartialWorker) updatePartialResult(ctx sessionctx.Context, sc *stmtctx.StatementContext, chk *chunk.Chunk, finalConcurrency int) (err error) {
	inputIter := chunk.NewIterator4Chunk(chk)
	for row := inputIter.Begin(); row != inputIter.End(); row = inputIter.Next() {
		groupKey, err := w.getGroupKey(sc, row)
		if err != nil {
			return errors.Trace(err)
		}
		partialResults := w.getPartialResult(sc, groupKey, w.partialResultsMap)
		for i, af := range w.aggFuncs {
			if err = af.UpdatePartialResult(ctx, []chunk.Row{row}, partialResults[i]); err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}

// shuffleIntermData shuffles the intermediate data of partial workers to corresponded final workers.
// We only support parallel execution for single-machine, so process of encode and decode can be skipped.
func (w *HashAggPartialWorker) shuffleIntermData(sc *stmtctx.StatementContext, finalConcurrency int) {
	groupKeysSlice := make([][]string, finalConcurrency)
	for groupKey := range w.partialResultsMap {
		finalWorkerIdx := int(murmur3.Sum32([]byte(groupKey))) % finalConcurrency
		if groupKeysSlice[finalWorkerIdx] == nil {
			groupKeysSlice[finalWorkerIdx] = make([]string, 0, len(w.partialResultsMap)/finalConcurrency)
		}
		groupKeysSlice[finalWorkerIdx] = append(groupKeysSlice[finalWorkerIdx], groupKey)
	}

	for i := range groupKeysSlice {
		if groupKeysSlice[i] == nil {
			continue
		}
		w.outputChs[i] <- &HashAggIntermData{
			groupKeys:        groupKeysSlice[i],
			partialResultMap: w.partialResultsMap,
		}
	}
}

// getGroupKey evaluates the group items and args of aggregate functions.
func (w *HashAggPartialWorker) getGroupKey(sc *stmtctx.StatementContext, row chunk.Row) ([]byte, error) {
	w.groupValDatums = w.groupValDatums[:0]
	for _, item := range w.groupByItems {
		v, err := item.Eval(row)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// This check is used to avoid error during the execution of `EncodeDecimal`.
		if item.GetType().Tp == mysql.TypeNewDecimal {
			v.SetLength(0)
		}
		w.groupValDatums = append(w.groupValDatums, v)
	}
	var err error
	w.groupKey, err = codec.EncodeValue(sc, w.groupKey[:0], w.groupValDatums...)
	return w.groupKey, errors.Trace(err)
}

func (w baseHashAggWorker) getPartialResult(sc *stmtctx.StatementContext, groupKey []byte, mapper aggPartialResultMapper) []aggfuncs.PartialResult {
	partialResults, ok := mapper[string(groupKey)]
	if !ok {
		partialResults = make([]aggfuncs.PartialResult, 0, len(w.aggFuncs))
		for _, af := range w.aggFuncs {
			partialResults = append(partialResults, af.AllocPartialResult())
		}
		mapper[string(groupKey)] = partialResults
	}
	return partialResults
}

func (w *HashAggFinalWorker) getPartialInput() (input *HashAggIntermData, ok bool) {
	select {
	case <-w.finishCh:
		return nil, false
	case input, ok = <-w.inputCh:
		if !ok {
			return nil, false
		}
	}
	return
}

func (w *HashAggFinalWorker) consumeIntermData(sctx sessionctx.Context) (err error) {
	var (
		input            *HashAggIntermData
		ok               bool
		intermDataBuffer [][]aggfuncs.PartialResult
		groupKeys        []string
		sc               = sctx.GetSessionVars().StmtCtx
	)
	for {
		if input, ok = w.getPartialInput(); !ok {
			return nil
		}
		if intermDataBuffer == nil {
			intermDataBuffer = make([][]aggfuncs.PartialResult, 0, w.maxChunkSize)
		}
		// Consume input in batches, size of every batch is less than w.maxChunkSize.
		for reachEnd := false; !reachEnd; {
			intermDataBuffer, groupKeys, reachEnd = input.getPartialResultBatch(sc, intermDataBuffer[:0], w.aggFuncs, w.maxChunkSize)
			for i, groupKey := range groupKeys {
				if !w.groupSet.Exist(groupKey) {
					w.groupSet.Insert(groupKey)
				}
				prs := intermDataBuffer[i]
				finalPartialResults := w.getPartialResult(sc, []byte(groupKey), w.partialResultMap)
				for j, af := range w.aggFuncs {
					if err = af.MergePartialResult(sctx, prs[j], finalPartialResults[j]); err != nil {
						return errors.Trace(err)
					}
				}
			}
		}
	}
}

func (w *HashAggFinalWorker) getFinalResult(sctx sessionctx.Context) {
	result, finished := w.receiveFinalResultHolder()
	if finished {
		return
	}
	result.Reset()
	for groupKey := range w.groupSet {
		partialResults := w.getPartialResult(sctx.GetSessionVars().StmtCtx, []byte(groupKey), w.partialResultMap)
		for i, af := range w.aggFuncs {
			af.AppendFinalResult2Chunk(sctx, partialResults[i], result)
		}
		if len(w.aggFuncs) == 0 {
			result.SetNumVirtualRows(result.NumRows() + 1)
		}
		if result.NumRows() == w.maxChunkSize {
			w.outputCh <- &AfFinalResult{chk: result, giveBackCh: w.finalResultHolderCh}
			result, finished = w.receiveFinalResultHolder()
			if finished {
				return
			}
			result.Reset()
		}
	}
	if result.NumRows() > 0 {
		w.outputCh <- &AfFinalResult{chk: result, giveBackCh: w.finalResultHolderCh}
	}
}

func (w *HashAggFinalWorker) receiveFinalResultHolder() (*chunk.Chunk, bool) {
	select {
	case <-w.finishCh:
		return nil, true
	case result, ok := <-w.finalResultHolderCh:
		return result, !ok
	}
}

func (w *HashAggFinalWorker) run(ctx sessionctx.Context, waitGroup *sync.WaitGroup) {
	defer func() {
		waitGroup.Done()
	}()
	if err := w.consumeIntermData(ctx); err != nil {
		w.outputCh <- &AfFinalResult{err: errors.Trace(err)}
	}
	w.getFinalResult(ctx)
}

// Next implements the Executor Next interface.
func (e *HashAggExec) Next(ctx context.Context, chk *chunk.Chunk) error {
	if e.runtimeStat != nil {
		start := time.Now()
		defer func() { e.runtimeStat.Record(time.Now().Sub(start), chk.NumRows()) }()
	}
	chk.Reset()
	if e.isUnparallelExec {
		return errors.Trace(e.unparallelExec(ctx, chk))
	}
	return errors.Trace(e.parallelExec(ctx, chk))
}

func (e *HashAggExec) fetchChildData(ctx context.Context) {
	var (
		input *HashAggInput
		chk   *chunk.Chunk
		ok    bool
		err   error
	)
	defer func() {
		for i := range e.partialInputChs {
			close(e.partialInputChs[i])
		}
	}()
	for {
		select {
		case <-e.finishCh:
			return
		case input, ok = <-e.inputCh:
			if !ok {
				return
			}
			chk = input.chk
		}
		err = e.children[0].Next(ctx, chk)
		if err != nil {
			e.finalOutputCh <- &AfFinalResult{err: errors.Trace(err)}
			return
		}
		if chk.NumRows() == 0 {
			return
		}
		input.giveBackCh <- chk
	}
}

func (e *HashAggExec) waitPartialWorkerAndCloseOutputChs(waitGroup *sync.WaitGroup) {
	waitGroup.Wait()
	for _, ch := range e.partialOutputChs {
		close(ch)
	}
}

func (e *HashAggExec) waitFinalWorkerAndCloseFinalOutput(waitGroup *sync.WaitGroup) {
	waitGroup.Wait()
	close(e.finalOutputCh)
}

func (e *HashAggExec) prepare4ParallelExec(ctx context.Context) {
	go e.fetchChildData(ctx)

	partialWorkerWaitGroup := &sync.WaitGroup{}
	partialWorkerWaitGroup.Add(len(e.partialWorkers))
	for i := range e.partialWorkers {
		go e.partialWorkers[i].run(e.ctx, partialWorkerWaitGroup, len(e.finalWorkers))
	}
	go e.waitPartialWorkerAndCloseOutputChs(partialWorkerWaitGroup)

	finalWorkerWaitGroup := &sync.WaitGroup{}
	finalWorkerWaitGroup.Add(len(e.finalWorkers))
	for i := range e.finalWorkers {
		go e.finalWorkers[i].run(e.ctx, finalWorkerWaitGroup)
	}
	go e.waitFinalWorkerAndCloseFinalOutput(finalWorkerWaitGroup)
}

// HashAggExec employs one input reader, M partial workers and N final workers to execute parallelly.
// The parallel execution flow is:
// 1. input reader reads data from child executor and send them to partial workers.
// 2. partial worker receives the input data, updates the partial results, and shuffle the partial results to the final workers.
// 3. final worker receives partial results from all the partial workers, evaluates the final results and sends the final results to the main thread.
func (e *HashAggExec) parallelExec(ctx context.Context, chk *chunk.Chunk) error {
	if !e.prepared {
		e.prepare4ParallelExec(ctx)
		e.prepared = true
	}
	for {
		result, ok := <-e.finalOutputCh
		if !ok || result.err != nil || result.chk.NumRows() == 0 {
			if result != nil {
				return errors.Trace(result.err)
			}
			if e.isChildReturnEmpty && e.defaultVal != nil {
				chk.Append(e.defaultVal, 0, 1)
			}
			e.isChildReturnEmpty = false
			return nil
		}
		e.isChildReturnEmpty = false
		chk.SwapColumns(result.chk)
		// Put result.chk back to the corresponded final worker's finalResultHolderCh.
		result.giveBackCh <- result.chk
		if chk.NumRows() > 0 {
			break
		}
	}
	return nil
}

// unparallelExec executes hash aggregation algorithm in single thread.
func (e *HashAggExec) unparallelExec(ctx context.Context, chk *chunk.Chunk) error {
	// In this stage we consider all data from src as a single group.
	if !e.prepared {
		err := e.execute(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		if (len(e.groupSet) == 0) && len(e.GroupByItems) == 0 {
			// If no groupby and no data, we should add an empty group.
			// For example:
			// "select count(c) from t;" should return one row [0]
			// "select count(c) from t group by c1;" should return empty result set.
			e.groupSet.Insert("")
			e.groupKeys = append(e.groupKeys, "")
		}
		e.prepared = true
	}
	chk.Reset()

	// Since we return e.maxChunkSize rows every time, so we should not traverse
	// `groupSet` because of its randomness.
	for ; e.cursor4GroupKey < len(e.groupKeys); e.cursor4GroupKey++ {
		partialResults := e.getPartialResults(e.groupKeys[e.cursor4GroupKey])
		if len(e.PartialAggFuncs) == 0 {
			chk.SetNumVirtualRows(chk.NumRows() + 1)
		}
		for i, af := range e.PartialAggFuncs {
			af.AppendFinalResult2Chunk(e.ctx, partialResults[i], chk)
		}
		if chk.NumRows() == e.maxChunkSize {
			e.cursor4GroupKey++
			return nil
		}
	}
	return nil
}

// execute fetches Chunks from src and update each aggregate function for each row in Chunk.
func (e *HashAggExec) execute(ctx context.Context) (err error) {
	inputIter := chunk.NewIterator4Chunk(e.childResult)
	for {
		err := e.children[0].Next(ctx, e.childResult)
		if err != nil {
			return errors.Trace(err)
		}
		// no more data.
		if e.childResult.NumRows() == 0 {
			return nil
		}
		for row := inputIter.Begin(); row != inputIter.End(); row = inputIter.Next() {
			groupKey, err := e.getGroupKey(row)
			if err != nil {
				return errors.Trace(err)
			}
			if !e.groupSet.Exist(groupKey) {
				e.groupSet.Insert(groupKey)
				e.groupKeys = append(e.groupKeys, groupKey)
			}
			partialResults := e.getPartialResults(groupKey)
			for i, af := range e.PartialAggFuncs {
				err = af.UpdatePartialResult(e.ctx, []chunk.Row{row}, partialResults[i])
				if err != nil {
					return errors.Trace(err)
				}
			}
		}
	}
}

func (e *HashAggExec) getGroupKey(row chunk.Row) (string, error) {
	e.groupValDatums = e.groupValDatums[:0]
	for _, item := range e.GroupByItems {
		v, err := item.Eval(row)
		if item.GetType().Tp == mysql.TypeNewDecimal {
			v.SetLength(0)
		}
		if err != nil {
			return "", errors.Trace(err)
		}
		e.groupValDatums = append(e.groupValDatums, v)
	}
	var err error
	e.groupKeyBuffer, err = codec.EncodeValue(e.sc, e.groupKeyBuffer[:0], e.groupValDatums...)
	if err != nil {
		return "", errors.Trace(err)
	}
	return string(e.groupKeyBuffer), nil
}

func (e *HashAggExec) getPartialResults(groupKey string) []aggfuncs.PartialResult {
	partialResults, ok := e.partialResultMap[groupKey]
	if !ok {
		partialResults = make([]aggfuncs.PartialResult, 0, len(e.PartialAggFuncs))
		for _, af := range e.PartialAggFuncs {
			partialResults = append(partialResults, af.AllocPartialResult())
		}
		e.partialResultMap[groupKey] = partialResults
	}
	return partialResults
}

// StreamAggExec deals with all the aggregate functions.
// It assumes all the input data is sorted by group by key.
// When Next() is called, it will return a result for the same group.
type StreamAggExec struct {
	baseExecutor

	executed bool
	// isChildReturnEmpty indicates whether the child executor only returns an empty input.
	isChildReturnEmpty bool
	defaultVal         *chunk.Chunk
	StmtCtx            *stmtctx.StatementContext
	GroupByItems       []expression.Expression
	curGroupKey        []types.Datum
	tmpGroupKey        []types.Datum
	inputIter          *chunk.Iterator4Chunk
	inputRow           chunk.Row
	aggFuncs           []aggfuncs.AggFunc
	partialResults     []aggfuncs.PartialResult
	groupRows          []chunk.Row
	childResult        *chunk.Chunk
}

// Open implements the Executor Open interface.
func (e *StreamAggExec) Open(ctx context.Context) error {
	if err := e.baseExecutor.Open(ctx); err != nil {
		return errors.Trace(err)
	}
	e.childResult = e.children[0].newFirstChunk()
	e.executed = false
	e.isChildReturnEmpty = true
	e.inputIter = chunk.NewIterator4Chunk(e.childResult)
	e.inputRow = e.inputIter.End()

	e.partialResults = make([]aggfuncs.PartialResult, 0, len(e.aggFuncs))
	for _, aggFunc := range e.aggFuncs {
		e.partialResults = append(e.partialResults, aggFunc.AllocPartialResult())
	}

	return nil
}

// Close implements the Executor Close interface.
func (e *StreamAggExec) Close() error {
	e.childResult = nil
	return errors.Trace(e.baseExecutor.Close())
}

// Next implements the Executor Next interface.
func (e *StreamAggExec) Next(ctx context.Context, chk *chunk.Chunk) error {
	if e.runtimeStat != nil {
		start := time.Now()
		defer func() { e.runtimeStat.Record(time.Now().Sub(start), chk.NumRows()) }()
	}
	chk.Reset()
	for !e.executed && chk.NumRows() < e.maxChunkSize {
		err := e.consumeOneGroup(ctx, chk)
		if err != nil {
			e.executed = true
			return errors.Trace(err)
		}
	}
	return nil
}

func (e *StreamAggExec) consumeOneGroup(ctx context.Context, chk *chunk.Chunk) error {
	for !e.executed {
		if err := e.fetchChildIfNecessary(ctx, chk); err != nil {
			return errors.Trace(err)
		}
		for ; e.inputRow != e.inputIter.End(); e.inputRow = e.inputIter.Next() {
			meetNewGroup, err := e.meetNewGroup(e.inputRow)
			if err != nil {
				return errors.Trace(err)
			}
			if meetNewGroup {
				err := e.consumeGroupRows()
				if err != nil {
					return errors.Trace(err)
				}
				err = e.appendResult2Chunk(chk)
				if err != nil {
					return errors.Trace(err)
				}
			}
			e.groupRows = append(e.groupRows, e.inputRow)
			if meetNewGroup {
				e.inputRow = e.inputIter.Next()
				return nil
			}
		}
	}
	return nil
}

func (e *StreamAggExec) consumeGroupRows() error {
	if len(e.groupRows) == 0 {
		return nil
	}

	for i, aggFunc := range e.aggFuncs {
		err := aggFunc.UpdatePartialResult(e.ctx, e.groupRows, e.partialResults[i])
		if err != nil {
			return errors.Trace(err)
		}
	}
	e.groupRows = e.groupRows[:0]
	return nil
}

func (e *StreamAggExec) fetchChildIfNecessary(ctx context.Context, chk *chunk.Chunk) (err error) {
	if e.inputRow != e.inputIter.End() {
		return nil
	}

	// Before fetching a new batch of input, we should consume the last group.
	err = e.consumeGroupRows()
	if err != nil {
		return errors.Trace(err)
	}

	err = e.children[0].Next(ctx, e.childResult)
	if err != nil {
		return errors.Trace(err)
	}

	// No more data.
	if e.childResult.NumRows() == 0 {
		if !e.isChildReturnEmpty {
			err = e.appendResult2Chunk(chk)
		} else if e.defaultVal != nil {
			chk.Append(e.defaultVal, 0, 1)
		}
		e.executed = true
		return errors.Trace(err)
	}
	// Reach here, "e.childrenResults[0].NumRows() > 0" is guaranteed.
	e.isChildReturnEmpty = false
	e.inputRow = e.inputIter.Begin()
	return nil
}

// appendResult2Chunk appends result of all the aggregation functions to the
// result chunk, and reset the evaluation context for each aggregation.
func (e *StreamAggExec) appendResult2Chunk(chk *chunk.Chunk) error {
	for i, aggFunc := range e.aggFuncs {
		err := aggFunc.AppendFinalResult2Chunk(e.ctx, e.partialResults[i], chk)
		if err != nil {
			return errors.Trace(err)
		}
		aggFunc.ResetPartialResult(e.partialResults[i])
	}
	if len(e.aggFuncs) == 0 {
		chk.SetNumVirtualRows(chk.NumRows() + 1)
	}
	return nil
}

// meetNewGroup returns a value that represents if the new group is different from last group.
func (e *StreamAggExec) meetNewGroup(row chunk.Row) (bool, error) {
	if len(e.GroupByItems) == 0 {
		return false, nil
	}
	e.tmpGroupKey = e.tmpGroupKey[:0]
	matched, firstGroup := true, false
	if len(e.curGroupKey) == 0 {
		matched, firstGroup = false, true
	}
	for i, item := range e.GroupByItems {
		v, err := item.Eval(row)
		if err != nil {
			return false, errors.Trace(err)
		}
		if matched {
			c, err := v.CompareDatum(e.StmtCtx, &e.curGroupKey[i])
			if err != nil {
				return false, errors.Trace(err)
			}
			matched = c == 0
		}
		e.tmpGroupKey = append(e.tmpGroupKey, v)
	}
	if matched {
		return false, nil
	}
	e.curGroupKey = e.curGroupKey[:0]
	for _, v := range e.tmpGroupKey {
		e.curGroupKey = append(e.curGroupKey, *((&v).Copy()))
	}
	return !firstGroup, nil
}
