//  Copyright 2017-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package base

import (
	"expvar"
	"fmt"
	"strconv"
	"sync"
	"time"
)

const (
	exportDebugExpvars = true
)

var TimingExpvarsEnabled = false

const (
	// StatsReplication (SGR 1.x)
	StatKeySgrActive                     = "sgr_active"
	StatKeySgrNumAttachmentsTransferred  = "sgr_num_attachments_transferred"
	StatKeySgrAttachmentBytesTransferred = "sgr_num_attachment_bytes_transferred"

	// StatsReplication (SGR 1.x and 2.x)
	StatKeySgrNumDocsPushed       = "sgr_num_docs_pushed"
	StatKeySgrNumDocsFailedToPush = "sgr_num_docs_failed_to_push"
	StatKeySgrDocsCheckedSent     = "sgr_docs_checked_sent"
)

const StatsGroupKeySyncGateway = "syncgateway"

var SyncGatewayStats SgwStats

func init() {
	// Initialize Sync Gateway Stats

	// All stats will be stored as part of this struct. Global variable accessible everywhere. To add stats see stats.go
	SyncGatewayStats = *NewSyncGatewayStats()

	// Publish our stats to expvars. This will run String method on SyncGatewayStats ( type SgwStats ) which will
	// marshal the stats to JSON
	expvar.Publish(StatsGroupKeySyncGateway, &SyncGatewayStats)
}

// Removes the per-database stats for this database by removing the database from the map
func RemovePerDbStats(dbName string) {

	// Clear out the stats for this db since they will no longer be updated.
	SyncGatewayStats.ClearDBStats(dbName)

}

// SequenceTimingExpvarMap attempts to track timing information for targeted sequences as they move through the system.
// Creates a map that looks like the following, where Indexed, Polled, Changes are the incoming stages, the values are
// nanosecond timestamps, and the sequences are the target sequences, based on the specified vb and frequency (in the example
// frequency=1000).  Since we won't necessarily see every vb sequence, we track the first sequence we see higher than the
// target frequency.  (e.g. if our last sequence was 1000 and frequency is 1000, it will track the first sequence seen higher than
// 2000).
// Note: Frequency needs to be high enough that a sequence can move through the system before the next sequence is seen, otherwise
// earlier stages could be updating current before the later stages have processed it.
/*
{
	"timingMap": {
		"seq1000.Indexed" :  4738432432,
		"seq1000.Polled" : 5743785947,
		"seq1000.Changes" :
		"seq2002.Indexed" :  4738432432,
		"seq2002.Polled" : 5743785947,
		"seq2002.Changes" :
	}
}
*/
type SequenceTimingExpvar struct {
	frequency        uint64
	currentTargetSeq uint64
	currentActualSeq uint64
	nextTargetSeq    uint64
	vbNo             uint16
	lock             sync.RWMutex
	timingMap        *expvar.Map
}

func NewSequenceTimingExpvar(frequency uint64, targetVbNo uint16, name string) SequenceTimingExpvar {

	storageMap := expvar.Map{}
	storageMap.Init()

	return SequenceTimingExpvar{
		currentTargetSeq: 0,
		nextTargetSeq:    0,
		frequency:        frequency,
		vbNo:             targetVbNo,
		timingMap:        &storageMap,
	}
}

type TimingStatus int

const (
	TimingStatusCurrent TimingStatus = iota
	TimingStatusNext
	TimingStatusNone
	TimingStatusInit
)

func (s *SequenceTimingExpvar) String() string {
	return s.timingMap.String()
}

func (s *SequenceTimingExpvar) UpdateBySequence(stage string, vbNo uint16, seq uint64) {

	if !TimingExpvarsEnabled {
		return
	}
	timingStatus := s.isCurrentOrNext(vbNo, seq)
	switch timingStatus {
	case TimingStatusNone:
		return
	case TimingStatusInit:
		s.initTiming(seq)
	case TimingStatusCurrent:
		s.setActual(seq)
		s.writeCurrentSeq(stage, time.Now())
	case TimingStatusNext:
		s.updateNext(stage, seq, time.Now())
	}
	return
}

func (s *SequenceTimingExpvar) UpdateBySequenceAt(stage string, vbNo uint16, seq uint64, time time.Time) {

	if !TimingExpvarsEnabled {
		return
	}
	timingStatus := s.isCurrentOrNext(vbNo, seq)
	switch timingStatus {
	case TimingStatusNone:
		return
	case TimingStatusInit:
		s.initTiming(seq)
	case TimingStatusCurrent:
		s.setActual(seq)
		s.writeCurrentSeq(stage, time)
	case TimingStatusNext:
		s.updateNext(stage, seq, time)
	}
	return
}

// Update by sequence range is used for events (like clock polling) that don't see
// every sequence.  Writes when current target sequence is in range.  Assumes callers
// don't report overlapping ranges
func (s *SequenceTimingExpvar) UpdateBySequenceRange(stage string, vbNo uint16, startSeq uint64, endSeq uint64) {

	if !TimingExpvarsEnabled {
		return
	}
	timingStatus := s.isCurrentOrNextRange(vbNo, startSeq, endSeq)
	switch timingStatus {
	case TimingStatusNone:
		return
	case TimingStatusInit:
		s.initTiming(endSeq)
	case TimingStatusCurrent:
		s.writeCurrentRange(stage)
	case TimingStatusNext:
		s.updateNextRange(stage, startSeq, endSeq)
	}
}

// Initializes based on the first sequence seen
func (s *SequenceTimingExpvar) initTiming(startSeq uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.nextTargetSeq == 0 {
		s.nextTargetSeq = ((startSeq / s.frequency) + 1) * s.frequency
	}
}

func (s *SequenceTimingExpvar) setActual(seq uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.currentActualSeq == 0 || s.currentActualSeq < s.currentTargetSeq {
		s.currentActualSeq = seq
	}
}

func (s *SequenceTimingExpvar) writeCurrentSeq(stage string, time time.Time) {

	key := fmt.Sprintf("seq%d:%s", s.currentTargetSeq, stage)
	value := expvar.Int{}
	value.Set(time.UnixNano())
	s.timingMap.Set(key, &value)
}

func (s *SequenceTimingExpvar) writeCurrentRange(stage string) {

	key := fmt.Sprintf("seq%d:%s", s.currentTargetSeq, stage)
	value := expvar.Int{}
	value.Set(time.Now().UnixNano())
	s.timingMap.Set(key, &value)
}

func (s *SequenceTimingExpvar) updateNext(stage string, seq uint64, time time.Time) {

	s.currentTargetSeq = s.nextTargetSeq
	s.currentActualSeq = seq
	s.nextTargetSeq = s.currentTargetSeq + s.frequency
	s.writeCurrentSeq(stage, time)
}

// UpdateNextRange updates the target values, but not actual
func (s *SequenceTimingExpvar) updateNextRange(stage string, fromSeq, toSeq uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.currentTargetSeq = s.nextTargetSeq
	s.nextTargetSeq = s.currentTargetSeq + s.frequency

	s.writeCurrentRange(stage)
}

func (s *SequenceTimingExpvar) isCurrentOrNextRange(vbNo uint16, startSeq uint64, endSeq uint64) TimingStatus {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if vbNo != s.vbNo {
		return TimingStatusNone
	}

	if s.nextTargetSeq == 0 {
		return TimingStatusInit
	}

	if startSeq <= s.nextTargetSeq && endSeq >= s.nextTargetSeq {
		return TimingStatusNext
	}

	if s.currentTargetSeq > 0 && startSeq <= s.currentTargetSeq && endSeq >= s.currentTargetSeq {
		return TimingStatusCurrent
	}

	return TimingStatusNone
}

func (s *SequenceTimingExpvar) isCurrentOrNext(vbNo uint16, seq uint64) TimingStatus {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if vbNo != s.vbNo {
		return TimingStatusNone
	}

	if s.nextTargetSeq == 0 {
		return TimingStatusInit
	}

	if seq > 0 {
		if seq > s.nextTargetSeq {
			return TimingStatusNext
		}
		// If matches actual
		if seq == s.currentActualSeq {
			return TimingStatusCurrent
		}
		// If actual hasn't been set yet
		if s.currentActualSeq < s.currentTargetSeq && seq >= s.currentTargetSeq {
			return TimingStatusCurrent
		}
	}

	return TimingStatusNone
}

// IntMean is an expvar.Value that returns the mean of all values that
// are sent via AddValue or AddSince.
type IntMeanVar struct {
	count int64 // number of values seen
	mean  int64 // average value
	mu    sync.RWMutex
}

func (v *IntMeanVar) String() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return strconv.FormatInt(v.mean, 10)
}

// Adds value.  Calculates new mean as iterative mean (avoids int overflow)
func (v *IntMeanVar) AddValue(value int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.count++
	v.mean = v.mean + int64((value-v.mean)/v.count)
}

func (v *IntMeanVar) AddSince(start time.Time) {
	v.AddValue(time.Since(start).Nanoseconds())
}

type DebugIntMeanVar struct {
	v IntMeanVar
}

func (d *DebugIntMeanVar) String() string {
	if exportDebugExpvars {
		return d.v.String()
	}
	return ""
}

func (d *DebugIntMeanVar) AddValue(value int64) {
	if exportDebugExpvars {
		d.v.AddValue(value)
	}
}

func (d *DebugIntMeanVar) AddSince(start time.Time) {
	if exportDebugExpvars {
		d.v.AddSince(start)
	}
}

// IntRollingMean is an expvar.Value that returns the mean of the [size] latest
// values sent via AddValue.  Uses a slice to track values, so setting a large
// size has memory implications
type IntRollingMeanVar struct {
	mean     float64 // average value
	mu       sync.RWMutex
	entries  []int64
	capacity int
	position int
}

func NewIntRollingMeanVar(capacity int) IntRollingMeanVar {
	return IntRollingMeanVar{
		capacity: capacity,
		entries:  make([]int64, 0, capacity),
	}
}

func (v *IntRollingMeanVar) String() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return strconv.FormatInt(int64(v.mean), 10)
}

// Adds value
func (v *IntRollingMeanVar) AddValue(value int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.entries) < v.capacity {
		v.addValue(value)
	} else {
		v.replaceValue(value)
	}
}

func (v *IntRollingMeanVar) AddSince(start time.Time) {
	v.AddValue(time.Since(start).Nanoseconds())
}

func (v *IntRollingMeanVar) AddSincePerItem(start time.Time, numItems int) {

	// avoid divide by zero errors
	if numItems == 0 {
		numItems = 1
	}

	// calculate per-item time delta
	timeDelta := time.Since(start).Nanoseconds()
	timeDeltaPerItem := timeDelta / int64(numItems)

	v.AddValue(timeDeltaPerItem)

}

// If we have fewer entries than capacity, regular mean calculation
func (v *IntRollingMeanVar) addValue(value int64) {
	v.entries = append(v.entries, value)
	v.mean = v.mean + (float64(value)-v.mean)/float64(len(v.entries))
}

// If we have filled the ring buffer, replace value at position and recalculate mean
func (v *IntRollingMeanVar) replaceValue(value int64) {
	oldValue := v.entries[v.position]
	v.entries[v.position] = value
	v.mean = v.mean + float64(value-oldValue)/float64(v.capacity)
	v.position++
	if v.position > v.capacity-1 {
		v.position = 0
	}
}
