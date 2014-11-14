// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"sync"
	"time"

	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/memdb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

var (
	errCompactionTransactExiting = errors.New("leveldb: compaction transact exiting")
)

type cStats struct {
	sync.Mutex
	duration time.Duration
	read     uint64
	write    uint64
}

func (p *cStats) add(n *cStatsStaging) {
	p.Lock()
	p.duration += n.duration
	p.read += n.read
	p.write += n.write
	p.Unlock()
}

func (p *cStats) get() (duration time.Duration, read, write uint64) {
	p.Lock()
	defer p.Unlock()
	return p.duration, p.read, p.write
}

type cStatsStaging struct {
	start    time.Time
	duration time.Duration
	on       bool
	read     uint64
	write    uint64
}

func (p *cStatsStaging) startTimer() {
	if !p.on {
		p.start = time.Now()
		p.on = true
	}
}

func (p *cStatsStaging) stopTimer() {
	if p.on {
		p.duration += time.Since(p.start)
		p.on = false
	}
}

type cMem struct {
	s     *session
	level int
	rec   *sessionRecord
}

func newCMem(s *session) *cMem {
	return &cMem{s: s, rec: &sessionRecord{numLevel: s.o.GetNumLevel()}}
}

func (c *cMem) flush(mem *memdb.DB, level int) error {
	s := c.s

	// Write memdb to table.
	iter := mem.NewIterator(nil)
	defer iter.Release()
	t, n, err := s.tops.createFrom(iter)
	if err != nil {
		return err
	}

	// Pick level.
	if level < 0 {
		v := s.version()
		level = v.pickLevel(t.imin.ukey(), t.imax.ukey())
		v.release()
	}
	c.rec.addTableFile(level, t)

	s.logf("mem@flush created L%d@%d N·%d S·%s %q:%q", level, t.file.Num(), n, shortenb(int(t.size)), t.imin, t.imax)

	c.level = level
	return nil
}

func (c *cMem) reset() {
	c.rec = &sessionRecord{numLevel: c.s.o.GetNumLevel()}
}

func (c *cMem) commit(journal, seq uint64) error {
	c.rec.setJournalNum(journal)
	c.rec.setSeqNum(seq)

	// Commit changes.
	return c.s.commit(c.rec)
}

func (db *DB) compactionError() {
	var (
		err     error
		wlocked bool
	)
noerr:
	// No error.
	for {
		select {
		case err = <-db.compErrSetC:
			switch {
			case err == nil:
			case errors.IsCorrupted(err):
				goto hasperr
			default:
				goto haserr
			}
		case _, _ = <-db.closeC:
			return
		}
	}
haserr:
	// Transient error.
	for {
		select {
		case db.compErrC <- err:
		case err = <-db.compErrSetC:
			switch {
			case err == nil:
				goto noerr
			case errors.IsCorrupted(err):
				goto hasperr
			default:
			}
		case _, _ = <-db.closeC:
			return
		}
	}
hasperr:
	// Persistent error.
	for {
		select {
		case db.compErrC <- err:
		case db.compPerErrC <- err:
		case db.writeLockC <- struct{}{}:
			// Hold write lock, so that write won't pass-through.
			wlocked = true
		case _, _ = <-db.closeC:
			if wlocked {
				// We should release the lock or Close will hang.
				<-db.writeLockC
			}
			return
		}
	}
}

type compactionTransactCounter int

func (cnt *compactionTransactCounter) incr() {
	*cnt++
}

func (db *DB) compactionTransact(name string, exec func(cnt *compactionTransactCounter) error, rollback func() error) {
	defer func() {
		if x := recover(); x != nil {
			if x == errCompactionTransactExiting && rollback != nil {
				if err := rollback(); err != nil {
					db.logf("%s rollback error %q", name, err)
				}
			}
			panic(x)
		}
	}()

	const (
		backoffMin = 1 * time.Second
		backoffMax = 8 * time.Second
		backoffMul = 2 * time.Second
	)
	var (
		backoff  = backoffMin
		backoffT = time.NewTimer(backoff)
		lastCnt  = compactionTransactCounter(0)

		disableBackoff = db.s.o.GetDisableCompactionBackoff()
	)
	for n := 0; ; n++ {
		// Check wether the DB is closed.
		if db.isClosed() {
			db.logf("%s exiting", name)
			db.compactionExitTransact()
		} else if n > 0 {
			db.logf("%s retrying N·%d", name, n)
		}

		// Execute.
		cnt := compactionTransactCounter(0)
		err := exec(&cnt)
		if err != nil {
			db.logf("%s error I·%d %q", name, cnt, err)
		}

		// Set compaction error status.
		select {
		case db.compErrSetC <- err:
		case perr := <-db.compPerErrC:
			if err != nil {
				db.logf("%s exiting (persistent error %q)", name, perr)
				db.compactionExitTransact()
			}
		case _, _ = <-db.closeC:
			db.logf("%s exiting", name)
			db.compactionExitTransact()
		}
		if err == nil {
			return
		}
		if errors.IsCorrupted(err) {
			db.logf("%s exiting (corruption detected)", name)
			db.compactionExitTransact()
		}

		if !disableBackoff {
			// Reset backoff duration if counter is advancing.
			if cnt > lastCnt {
				backoff = backoffMin
				lastCnt = cnt
			}

			// Backoff.
			backoffT.Reset(backoff)
			if backoff < backoffMax {
				backoff *= backoffMul
				if backoff > backoffMax {
					backoff = backoffMax
				}
			}
			select {
			case <-backoffT.C:
			case _, _ = <-db.closeC:
				db.logf("%s exiting", name)
				db.compactionExitTransact()
			}
		}
	}
}

func (db *DB) compactionExitTransact() {
	panic(errCompactionTransactExiting)
}

func (db *DB) memCompaction() {
	mem := db.getFrozenMem()
	if mem == nil {
		return
	}
	defer mem.decref()

	c := newCMem(db.s)
	stats := new(cStatsStaging)

	db.logf("mem@flush N·%d S·%s", mem.mdb.Len(), shortenb(mem.mdb.Size()))

	// Don't compact empty memdb.
	if mem.mdb.Len() == 0 {
		db.logf("mem@flush skipping")
		// drop frozen mem
		db.dropFrozenMem()
		return
	}

	// Pause table compaction.
	resumeC := make(chan struct{})
	select {
	case db.tcompPauseC <- (chan<- struct{})(resumeC):
	case <-db.compPerErrC:
		close(resumeC)
		resumeC = nil
	case _, _ = <-db.closeC:
		return
	}

	db.compactionTransact("mem@flush", func(cnt *compactionTransactCounter) (err error) {
		stats.startTimer()
		defer stats.stopTimer()
		return c.flush(mem.mdb, -1)
	}, func() error {
		for _, r := range c.rec.addedTables {
			db.logf("mem@flush rollback @%d", r.num)
			f := db.s.getTableFile(r.num)
			if err := f.Remove(); err != nil {
				return err
			}
		}
		return nil
	})

	db.compactionTransact("mem@commit", func(cnt *compactionTransactCounter) (err error) {
		stats.startTimer()
		defer stats.stopTimer()
		return c.commit(db.journalFile.Num(), db.frozenSeq)
	}, nil)

	db.logf("mem@flush committed F·%d T·%v", len(c.rec.addedTables), stats.duration)

	for _, r := range c.rec.addedTables {
		stats.write += r.size
	}
	db.compStats[c.level].add(stats)

	// Drop frozen mem.
	db.dropFrozenMem()

	// Resume table compaction.
	if resumeC != nil {
		select {
		case <-resumeC:
			close(resumeC)
		case _, _ = <-db.closeC:
			return
		}
	}

	// Trigger table compaction.
	db.compSendTrigger(db.tcompCmdC)
}

func (db *DB) tableCompaction(c *compaction, noTrivial bool) {
	defer c.release()

	rec := &sessionRecord{numLevel: db.s.o.GetNumLevel()}
	rec.addCompPtr(c.level, c.imax)

	if !noTrivial && c.trivial() {
		t := c.tables[0][0]
		db.logf("table@move L%d@%d -> L%d", c.level, t.file.Num(), c.level+1)
		rec.delTable(c.level, t.file.Num())
		rec.addTableFile(c.level+1, t)
		db.compactionTransact("table@move", func(cnt *compactionTransactCounter) (err error) {
			return db.s.commit(rec)
		}, nil)
		return
	}

	var stats [2]cStatsStaging
	for i, tables := range c.tables {
		for _, t := range tables {
			stats[i].read += t.size
			// Insert deleted tables into record
			rec.delTable(c.level+i, t.file.Num())
		}
	}
	sourceSize := int(stats[0].read + stats[1].read)
	minSeq := db.minSeq()
	db.logf("table@compaction L%d·%d -> L%d·%d S·%s Q·%d", c.level, len(c.tables[0]), c.level+1, len(c.tables[1]), shortenb(sourceSize), minSeq)

	var (
		snapHasLastUkey bool
		snapLastUkey    []byte
		snapLastSeq     uint64
		snapIter        int
		snapKerrCnt     int
		snapDropCnt     int

		kerrCnt int
		dropCnt int

		strict    = db.s.o.GetStrict(opt.StrictCompaction)
		tableSize = db.s.o.GetCompactionTableSize(c.level + 1)
	)
	db.compactionTransact("table@build", func(cnt *compactionTransactCounter) (err error) {
		hasLastUkey := snapHasLastUkey // The key might has zero length, so this is necessary.
		lastUkey := append([]byte{}, snapLastUkey...)
		lastSeq := snapLastSeq
		kerrCnt = snapKerrCnt
		dropCnt = snapDropCnt
		snapSched := snapIter == 0

		var tw *tWriter
		finish := func() error {
			t, err := tw.finish()
			if err != nil {
				return err
			}
			rec.addTableFile(c.level+1, t)
			stats[1].write += t.size
			db.logf("table@build created L%d@%d N·%d S·%s %q:%q", c.level+1, t.file.Num(), tw.tw.EntriesLen(), shortenb(int(t.size)), t.imin, t.imax)
			return nil
		}

		defer func() {
			stats[1].stopTimer()
			if tw != nil {
				tw.drop()
				tw = nil
			}
		}()

		stats[1].startTimer()
		iter := c.newIterator()
		defer iter.Release()
		for i := 0; iter.Next(); i++ {
			// Incr transact counter.
			cnt.incr()

			// Skip until last state.
			if i < snapIter {
				continue
			}

			ikey := iter.Key()
			ukey, seq, kt, kerr := parseIkey(ikey)

			// Skip this if key is corrupted.
			if kerr == nil && c.shouldStopBefore(ikey) && tw != nil {
				err = finish()
				if err != nil {
					return
				}
				snapSched = true
				tw = nil
			}

			// Scheduled for snapshot, snapshot will used to retry compaction
			// if error occured.
			if snapSched {
				snapHasLastUkey = hasLastUkey
				snapLastUkey = append(snapLastUkey[:0], lastUkey...)
				snapLastSeq = lastSeq
				snapIter = i
				snapKerrCnt = kerrCnt
				snapDropCnt = dropCnt
				snapSched = false
			}

			if kerr == nil {
				if !hasLastUkey || db.s.icmp.uCompare(lastUkey, ukey) != 0 {
					// First occurrence of this user key.
					hasLastUkey = true
					lastUkey = append(lastUkey[:0], ukey...)
					lastSeq = kMaxSeq
				}

				switch {
				case lastSeq <= minSeq:
					// Dropped because newer entry for same user key exist
					fallthrough // (A)
				case kt == ktDel && seq <= minSeq && c.baseLevelForKey(lastUkey):
					// For this user key:
					// (1) there is no data in higher levels
					// (2) data in lower levels will have larger seq numbers
					// (3) data in layers that are being compacted here and have
					//     smaller seq numbers will be dropped in the next
					//     few iterations of this loop (by rule (A) above).
					// Therefore this deletion marker is obsolete and can be dropped.
					lastSeq = seq
					dropCnt++
					continue
				default:
					lastSeq = seq
				}
			} else {
				if strict {
					return kerr
				}

				// Don't drop corrupted keys
				hasLastUkey = false
				lastUkey = lastUkey[:0]
				lastSeq = kMaxSeq
				kerrCnt++
			}

			// Create new table if not already
			if tw == nil {
				// Check for pause event.
				select {
				case ch := <-db.tcompPauseC:
					db.pauseCompaction(ch)
				case _, _ = <-db.closeC:
					db.compactionExitTransact()
				default:
				}

				// Create new table.
				tw, err = db.s.tops.create()
				if err != nil {
					return
				}
			}

			// Write key/value into table
			err = tw.append(ikey, iter.Value())
			if err != nil {
				return
			}

			// Finish table if it is big enough
			if tw.tw.BytesLen() >= tableSize {
				err = finish()
				if err != nil {
					return
				}
				snapSched = true
				tw = nil
			}
		}

		err = iter.Error()
		if err != nil {
			return
		}

		// Finish last table
		if tw != nil && !tw.empty() {
			err = finish()
			if err != nil {
				return
			}
			tw = nil
		}
		return
	}, func() error {
		for _, r := range rec.addedTables {
			db.logf("table@build rollback @%d", r.num)
			f := db.s.getTableFile(r.num)
			if err := f.Remove(); err != nil {
				return err
			}
		}
		return nil
	})

	// Commit changes
	db.compactionTransact("table@commit", func(cnt *compactionTransactCounter) (err error) {
		stats[1].startTimer()
		defer stats[1].stopTimer()
		return db.s.commit(rec)
	}, nil)

	resultSize := int(stats[1].write)
	db.logf("table@compaction committed F%s S%s Ke·%d D·%d T·%v", sint(len(rec.addedTables)-len(rec.deletedTables)), sshortenb(resultSize-sourceSize), kerrCnt, dropCnt, stats[1].duration)

	// Save compaction stats
	for i := range stats {
		db.compStats[c.level+1].add(&stats[i])
	}
}

func (db *DB) tableRangeCompaction(level int, umin, umax []byte) {
	db.logf("table@compaction range L%d %q:%q", level, umin, umax)

	if level >= 0 {
		if c := db.s.getCompactionRange(level, umin, umax); c != nil {
			db.tableCompaction(c, true)
		}
	} else {
		v := db.s.version()
		m := 1
		for i, t := range v.tables[1:] {
			if t.overlaps(db.s.icmp, umin, umax, false) {
				m = i + 1
			}
		}
		v.release()

		for level := 0; level < m; level++ {
			if c := db.s.getCompactionRange(level, umin, umax); c != nil {
				db.tableCompaction(c, true)
			}
		}
	}
}

func (db *DB) tableAutoCompaction() {
	if c := db.s.pickCompaction(); c != nil {
		db.tableCompaction(c, false)
	}
}

func (db *DB) tableNeedCompaction() bool {
	v := db.s.version()
	defer v.release()
	return v.needCompaction()
}

func (db *DB) pauseCompaction(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	case _, _ = <-db.closeC:
		db.compactionExitTransact()
	}
}

type cCmd interface {
	ack(err error)
}

type cIdle struct {
	ackC chan<- error
}

func (r cIdle) ack(err error) {
	if r.ackC != nil {
		defer func() {
			recover()
		}()
		r.ackC <- err
	}
}

type cRange struct {
	level    int
	min, max []byte
	ackC     chan<- error
}

func (r cRange) ack(err error) {
	if r.ackC != nil {
		defer func() {
			recover()
		}()
		r.ackC <- err
	}
}

// This will trigger auto compation and/or wait for all compaction to be done.
func (db *DB) compSendIdle(compC chan<- cCmd) (err error) {
	ch := make(chan error)
	defer close(ch)
	// Send cmd.
	select {
	case compC <- cIdle{ch}:
	case err = <-db.compErrC:
		return
	case _, _ = <-db.closeC:
		return ErrClosed
	}
	// Wait cmd.
	select {
	case err = <-ch:
	case err = <-db.compErrC:
	case _, _ = <-db.closeC:
		return ErrClosed
	}
	return err
}

// This will trigger auto compaction but will not wait for it.
func (db *DB) compSendTrigger(compC chan<- cCmd) {
	select {
	case compC <- cIdle{}:
	default:
	}
}

// Send range compaction request.
func (db *DB) compSendRange(compC chan<- cCmd, level int, min, max []byte) (err error) {
	ch := make(chan error)
	defer close(ch)
	// Send cmd.
	select {
	case compC <- cRange{level, min, max, ch}:
	case err := <-db.compErrC:
		return err
	case _, _ = <-db.closeC:
		return ErrClosed
	}
	// Wait cmd.
	select {
	case err = <-ch:
	case err = <-db.compErrC:
	case _, _ = <-db.closeC:
		return ErrClosed
	}
	return err
}

func (db *DB) mCompaction() {
	var x cCmd

	defer func() {
		if x := recover(); x != nil {
			if x != errCompactionTransactExiting {
				panic(x)
			}
		}
		if x != nil {
			x.ack(ErrClosed)
		}
		db.closeW.Done()
	}()

	for {
		select {
		case x = <-db.mcompCmdC:
			switch x.(type) {
			case cIdle:
				db.memCompaction()
				x.ack(nil)
				x = nil
			default:
				panic("leveldb: unknown command")
			}
		case _, _ = <-db.closeC:
			return
		}
	}
}

func (db *DB) tCompaction() {
	var x cCmd
	var ackQ []cCmd

	defer func() {
		if x := recover(); x != nil {
			if x != errCompactionTransactExiting {
				panic(x)
			}
		}
		for i := range ackQ {
			ackQ[i].ack(ErrClosed)
			ackQ[i] = nil
		}
		if x != nil {
			x.ack(ErrClosed)
		}
		db.closeW.Done()
	}()

	for {
		if db.tableNeedCompaction() {
			select {
			case x = <-db.tcompCmdC:
			case ch := <-db.tcompPauseC:
				db.pauseCompaction(ch)
				continue
			case _, _ = <-db.closeC:
				return
			default:
			}
		} else {
			for i := range ackQ {
				ackQ[i].ack(nil)
				ackQ[i] = nil
			}
			ackQ = ackQ[:0]
			select {
			case x = <-db.tcompCmdC:
			case ch := <-db.tcompPauseC:
				db.pauseCompaction(ch)
				continue
			case _, _ = <-db.closeC:
				return
			}
		}
		if x != nil {
			switch cmd := x.(type) {
			case cIdle:
				ackQ = append(ackQ, x)
			case cRange:
				db.tableRangeCompaction(cmd.level, cmd.min, cmd.max)
				x.ack(nil)
			default:
				panic("leveldb: unknown command")
			}
			x = nil
		}
		db.tableAutoCompaction()
	}
}
