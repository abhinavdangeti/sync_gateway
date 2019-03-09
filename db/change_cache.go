package db

import (
	"container/heap"
	"container/list"
	"errors"
	"expvar"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	sgbucket "github.com/couchbase/sg-bucket"
	"github.com/couchbase/sync_gateway/auth"
	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/channels"
)

const (
	DefaultCachePendingSeqMaxNum  = 10000            // Max number of waiting sequences
	DefaultCachePendingSeqMaxWait = 5 * time.Second  // Max time we'll wait for a pending sequence before sending to missed queue
	DefaultSkippedSeqMaxWait      = 60 * time.Minute // Max time we'll wait for an entry in the missing before purging
)

var SkippedSeqCleanViewBatch = 50 // Max number of sequences checked per query during CleanSkippedSequence.  Var to support testing

// Enable keeping a channel-log for the "*" channel (channel.UserStarChannel). The only time this channel is needed is if
// someone has access to "*" (e.g. admin-party) and tracks its changes feed.
var EnableStarChannelLog = true

var changeCacheExpvars *expvar.Map

func init() {
	changeCacheExpvars = expvar.NewMap("syncGateway_changeCache")
	changeCacheExpvars.Set("maxPending", new(base.IntMax))
}

// Manages a cache of the recent change history of all channels.
//
// Core responsibilities:
//
// - Manage collection of channel caches
// - Receive DCP changes via callbacks
//    - Perform sequence buffering to ensure documents are received in sequence order
//    - Propagating DCP changes down to appropriate channel caches
type changeCache struct {
	context         *DatabaseContext
	logsDisabled    bool                     // If true, ignore incoming tap changes
	nextSequence    uint64                   // Next consecutive sequence number to add.  State variable for sequence buffering tracking.  Should use getNextSequence() rather than accessing directly.
	initialSequence uint64                   // DB's current sequence at startup time. Should use getInitialSequence() rather than accessing directly.
	receivedSeqs    map[uint64]struct{}      // Set of all sequences received
	pendingLogs     LogPriorityQueue         // Out-of-sequence entries waiting to be cached
	channelCaches   map[string]*channelCache // A cache of changes for each channel
	notifyChange    func(base.Set)           // Client callback that notifies of channel changes
	stopped         bool                     // Set by the Stop method
	skippedSeqs     *SkippedSequenceList     // Skipped sequences still pending on the TAP feed
	skippedSeqLock  sync.RWMutex             // Coordinates access to skippedSeqs queue
	lock            sync.RWMutex             // Coordinates access to struct fields
	lateSeqLock     sync.RWMutex             // Coordinates access to late sequence caches
	options         CacheOptions             // Cache config
	terminator      chan bool                // Signal termination of background goroutines
}

type LogEntry channels.LogEntry

func (l LogEntry) String() string {
	return channels.LogEntry(l).String()
}

type LogEntries []*LogEntry

// A priority-queue of LogEntries, kept ordered by increasing sequence #.
type LogPriorityQueue []*LogEntry

type SkippedSequence struct {
	seq       uint64
	timeAdded time.Time
}

type CacheOptions struct {
	ChannelCacheOptions
	CachePendingSeqMaxWait time.Duration // Max wait for pending sequence before skipping
	CachePendingSeqMaxNum  int           // Max number of pending sequences before skipping
	CacheSkippedSeqMaxWait time.Duration // Max wait for skipped sequence before abandoning
}

//////// HOUSEKEEPING:

// Initializes a new changeCache.
// lastSequence is the last known database sequence assigned.
// notifyChange is an optional function that will be called to notify of channel changes.
// After calling Init(), you must call .Start() to start useing the cache, otherwise it will be in a locked state
// and callers will block on trying to obtain the lock.
func (c *changeCache) Init(context *DatabaseContext, notifyChange func(base.Set), options *CacheOptions, indexOptions *ChannelIndexOptions) error {
	c.context = context

	c.notifyChange = notifyChange
	c.channelCaches = make(map[string]*channelCache, 10)
	c.receivedSeqs = make(map[uint64]struct{})
	c.terminator = make(chan bool)
	c.skippedSeqs = NewSkippedSequenceList()

	// init cache options
	c.options = CacheOptions{
		CachePendingSeqMaxWait: DefaultCachePendingSeqMaxWait,
		CachePendingSeqMaxNum:  DefaultCachePendingSeqMaxNum,
		CacheSkippedSeqMaxWait: DefaultSkippedSeqMaxWait,
	}

	if options != nil {
		if options.CachePendingSeqMaxNum > 0 {
			c.options.CachePendingSeqMaxNum = options.CachePendingSeqMaxNum
		}

		if options.CachePendingSeqMaxWait > 0 {
			c.options.CachePendingSeqMaxWait = options.CachePendingSeqMaxWait
		}

		if options.CacheSkippedSeqMaxWait > 0 {
			c.options.CacheSkippedSeqMaxWait = options.CacheSkippedSeqMaxWait
		}
		c.options.ChannelCacheOptions = options.ChannelCacheOptions
	}

	base.Infof(base.KeyCache, "Initializing changes cache with options %+v", c.options)

	if context.UseGlobalSequence() {
		base.Infof(base.KeyAll, "Initializing changes cache for database %s", base.UD(context.Name))
	}

	heap.Init(&c.pendingLogs)

	// Start a background task for periodic housekeeping:
	go func() {
		for {
			select {
			case <-time.After(c.options.CachePendingSeqMaxWait / 2):
				c.CleanUp()
			case <-c.terminator:
				return
			}
		}
	}()

	// Start a background task for SkippedSequenceQueue housekeeping:
	go func() {
		for {
			select {
			case <-time.After(c.options.CacheSkippedSeqMaxWait / 2):
				c.CleanSkippedSequenceQueue()
			case <-c.terminator:
				return
			}
		}
	}()

	// Lock the cache -- not usable until .Start() called.  This fixes the DCP startup race condition documented in SG #3558.
	c.lock.Lock()

	return nil
}

func (c *changeCache) Start() error {

	// Unlock the cache after this function returns.
	defer c.lock.Unlock()

	// Find the current global doc sequence and use that for the initial sequence for the change cache
	lastSequence, err := c.context.LastSequence()
	if err != nil {
		return err
	}

	c._setInitialSequence(lastSequence)
	return nil
}

// Stops the cache. Clears its state and tells the housekeeping task to stop.
func (c *changeCache) Stop() {

	// Signal to background goroutines that the changeCache has been stopped, so they can exit
	// their loop
	close(c.terminator)

	c.lock.Lock()
	c.stopped = true
	c.logsDisabled = true
	c.lock.Unlock()
}

func (c *changeCache) IsStopped() bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.stopped
}

// Empty out all channel caches.
func (c *changeCache) Clear() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Reset initialSequence so that any new channel caches have their validFrom set to the current last sequence
	// the point at which the change cache was initialized / re-initialized.
	// No need to touch c.nextSequence here, because we don't want to touch the sequence buffering state.
	var err error
	c.initialSequence, err = c.context.LastSequence()
	if err != nil {
		return err
	}

	c.channelCaches = make(map[string]*channelCache, 10)
	c.pendingLogs = nil
	heap.Init(&c.pendingLogs)

	return nil
}

// If set to false, DocChanged() becomes a no-op.
func (c *changeCache) EnableChannelIndexing(enable bool) {
	c.lock.Lock()
	c.logsDisabled = !enable
	c.lock.Unlock()
}

// Cleanup function, invoked periodically.
// Inserts pending entries that have been waiting too long.
// Removes entries older than MaxChannelLogCacheAge from the cache.
// Returns false if the changeCache has been closed.
func (c *changeCache) CleanUp() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.channelCaches == nil {
		return false
	}

	// If entries have been pending too long, add them to the cache:
	changedChannels := c._addPendingLogs()
	if c.notifyChange != nil && len(changedChannels) > 0 {
		c.notifyChange(changedChannels)
	}

	// Remove old cache entries:
	for channelName := range c.channelCaches {
		c._getChannelCache(channelName).pruneCache()
	}
	return true
}

// Cleanup function, invoked periodically.
// Removes skipped entries from skippedSeqs that have been waiting longer
// than MaxChannelLogMissingWaitTime from the queue.  Attempts view retrieval
// prior to removal.  Only locks skipped sequence queue to build the initial set (GetSkippedSequencesOlderThanMaxWait)
// and subsequent removal (RemoveSkipped).
func (c *changeCache) CleanSkippedSequenceQueue() {

	oldSkippedSequences := c.GetSkippedSequencesOlderThanMaxWait()
	if len(oldSkippedSequences) == 0 {
		return
	}

	base.Infof(base.KeyCache, "Starting CleanSkippedSequenceQueue, found %d skipped sequences older than max wait for database %s", len(oldSkippedSequences), base.UD(c.context.Name))

	var foundEntries []*LogEntry
	var pendingRemovals []uint64

	if c.context.Options.UnsupportedOptions.DisableCleanSkippedQuery == true {
		pendingRemovals = append(pendingRemovals, oldSkippedSequences...)
		oldSkippedSequences = nil
	}

	for len(oldSkippedSequences) > 0 {
		var skippedSeqBatch []uint64
		if len(oldSkippedSequences) >= SkippedSeqCleanViewBatch {
			skippedSeqBatch = oldSkippedSequences[0:SkippedSeqCleanViewBatch]
			oldSkippedSequences = oldSkippedSequences[SkippedSeqCleanViewBatch:]
		} else {
			skippedSeqBatch = oldSkippedSequences[0:]
			oldSkippedSequences = nil
		}

		base.Infof(base.KeyCache, "Issuing skipped sequence clean query for %d sequences, %d remain pending (db:%s).", len(skippedSeqBatch), len(oldSkippedSequences), base.UD(c.context.Name))
		// Note: The view query is only going to hit for active revisions - sequences associated with inactive revisions
		//       aren't indexed by the channel view.  This means we can potentially miss channel removals:
		//       when an older revision is missed by the TAP feed, and a channel is removed in that revision,
		//       the doc won't be flagged as removed from that channel in the in-memory channel cache.
		entries, err := c.context.getChangesForSequences(skippedSeqBatch)
		if err != nil {
			base.Warnf(base.KeyAll, "Error retrieving sequences via view during skipped sequence clean - #%d sequences treated as not found: %v", len(skippedSeqBatch), err)
			continue
		}

		// Process found entries.  Add to foundEntries for subsequent cache processing, foundMap for pendingRemoval calculation below.
		foundMap := make(map[uint64]struct{}, len(entries))
		for _, foundEntry := range entries {
			foundMap[foundEntry.Sequence] = struct{}{}
			foundEntries = append(foundEntries, foundEntry)
		}

		// Add queried sequences not in the resultset to pendingRemovals
		for _, skippedSeq := range skippedSeqBatch {
			if _, ok := foundMap[skippedSeq]; !ok {
				base.Warnf(base.KeyAll, "Skipped Sequence %d didn't show up in MaxChannelLogMissingWaitTime, and isn't available from the * channel view.  If it's a valid sequence, it won't be replicated until Sync Gateway is restarted.", skippedSeq)
				pendingRemovals = append(pendingRemovals, skippedSeq)
			}
		}
	}

	// Issue processEntry for found entries.  Standard processEntry handling will remove these sequences from the skipped seq queue.
	changedChannelsCombined := base.Set{}
	for _, entry := range foundEntries {
		entry.Skipped = true
		// Need to populate the actual channels for this entry - the entry returned from the * channel
		// view will only have the * channel
		doc, err := c.context.GetDocument(entry.DocID, DocUnmarshalNoHistory)
		if err != nil {
			base.Warnf(base.KeyAll, "Unable to retrieve doc when processing skipped document %q: abandoning sequence %d", base.UD(entry.DocID), entry.Sequence)
			continue
		}
		entry.Channels = doc.Channels

		changedChannels := c.processEntry(entry)
		changedChannelsCombined = changedChannelsCombined.Update(changedChannels)
	}

	// Since the calls to processEntry() above may unblock pending sequences, if there were any changed channels we need
	// to notify any change listeners that are working changes feeds for these channels
	if c.notifyChange != nil && len(changedChannelsCombined) > 0 {
		c.notifyChange(changedChannelsCombined)
	}

	// Purge sequences not found from the skipped sequence queue
	numRemoved := c.RemoveSkippedSequences(pendingRemovals)
	dbExpvars.Add("abandoned_seqs", numRemoved)

	base.Infof(base.KeyCache, "CleanSkippedSequenceQueue complete.  Found:%d, Not Found:%d for database %s.", len(foundEntries), len(pendingRemovals), base.UD(c.context.Name))
	return
}

//////// ADDING CHANGES:

// Given a newly changed document (received from the tap feed), adds change entries to channels.
// The JSON must be the raw document from the bucket, with the metadata and all.
func (c *changeCache) DocChanged(event sgbucket.FeedEvent) {

	if event.Synchronous {
		c.DocChangedSynchronous(event)
	} else {
		go c.DocChangedSynchronous(event)
	}
}

// Note that DocChangedSynchronous may be executed concurrently for multiple events (in the DCP case, DCP events
// originating from multiple vbuckets).  Only processEntry is locking - all other functionality needs to support
// concurrent processing.
func (c *changeCache) DocChangedSynchronous(event sgbucket.FeedEvent) {
	entryTime := time.Now()
	docID := string(event.Key)
	docJSON := event.Value
	changedChannelsCombined := base.Set{}

	// ** This method does not directly access any state of c, so it doesn't lock.
	// Is this a user/role doc?
	if strings.HasPrefix(docID, auth.UserKeyPrefix) {
		c.processPrincipalDoc(docID, docJSON, true)
		return
	} else if strings.HasPrefix(docID, auth.RoleKeyPrefix) {
		c.processPrincipalDoc(docID, docJSON, false)
		return
	}

	// Is this an unused sequence notification?
	if strings.HasPrefix(docID, UnusedSequenceKeyPrefix) {
		c.processUnusedSequence(docID)
		return
	}
	if strings.HasPrefix(docID, UnusedSequenceRangeKeyPrefix) {
		c.processUnusedSequenceRange(docID)
		return
	}

	// If this is a delete and there are no xattrs (no existing SG revision), we can ignore
	if event.Opcode == sgbucket.FeedOpDeletion && len(docJSON) == 0 {
		base.Debugf(base.KeyImport, "Ignoring delete mutation for %s - no existing Sync Gateway metadata.", base.UD(docID))
		return
	}

	// If this is a binary document (and not one of the above types), we can ignore.  Currently only performing this check when xattrs
	// are enabled, because walrus doesn't support DataType on feed.
	if c.context.UseXattrs() && event.DataType == base.MemcachedDataTypeRaw {
		return
	}

	// First unmarshal the doc (just its metadata, to save time/memory):
	syncData, rawBody, rawXattr, err := UnmarshalDocumentSyncDataFromFeed(docJSON, event.DataType, false)
	if err != nil {
		// Avoid log noise related to failed unmarshaling of binary documents.
		if event.DataType != base.MemcachedDataTypeRaw {
			base.Debugf(base.KeyCache, "Unable to unmarshal sync metadata for feed document %q.  Will not be included in channel cache.  Error: %v", base.UD(docID), err)
		}
		if err == base.ErrEmptyMetadata {
			base.Warnf(base.KeyAll, "Unexpected empty metadata when processing feed event.  docid: %s opcode: %v datatype:%v", base.UD(event.Key), event.Opcode, event.DataType)
		}
		return
	}

	// Import handling.
	if c.context.UseXattrs() {
		// If this isn't an SG write, we shouldn't attempt to cache.  Import if this node is configured for import, otherwise ignore.
		if syncData == nil || !syncData.IsSGWrite(event.Cas, rawBody) {
			if c.context.autoImport {
				// If syncData is nil, or if this was not an SG write, attempt to import
				isDelete := event.Opcode == sgbucket.FeedOpDeletion
				if isDelete {
					rawBody = nil
				}

				db := Database{DatabaseContext: c.context, user: nil}
				_, err := db.ImportDocRaw(docID, rawBody, rawXattr, isDelete, event.Cas, &event.Expiry, ImportFromFeed)
				if err != nil {
					if err == base.ErrImportCasFailure {
						base.Debugf(base.KeyImport, "Not importing mutation - document %s has been subsequently updated and will be imported based on that mutation.", base.UD(docID))
					} else if err == base.ErrImportCancelledFilter {
						// No logging required - filter info already logged during importDoc
					} else {
						base.Debugf(base.KeyImport, "Did not import doc %q - external update will not be accessible via Sync Gateway.  Reason: %v", base.UD(docID), err)
					}
				}
			}
			return
		}
	}

	if !syncData.HasValidSyncData(c.context.writeSequences()) {
		// No sync metadata found - check whether we're mid-upgrade and attempting to read a doc w/ metadata stored in xattr
		migratedDoc, _ := c.context.checkForUpgrade(docID)
		if migratedDoc != nil && migratedDoc.Cas == event.Cas {
			base.Infof(base.KeyCache, "Found mobile xattr on document without _sync property - caching, assuming upgrade in progress.")
			syncData = &migratedDoc.syncData
		} else {
			base.Warnf(base.KeyAll, "changeCache: Doc %q does not have valid sync data.", base.UD(docID))
			return
		}
	}

	if syncData.Sequence <= c.getInitialSequence() {
		return // Tap is sending us an old value from before I started up; ignore it
	}

	// Record a histogram of the Tap feed's lag:
	tapLag := time.Since(syncData.TimeSaved) - time.Since(entryTime)

	// If the doc update wasted any sequences due to conflicts, add empty entries for them:
	for _, seq := range syncData.UnusedSequences {
		base.Infof(base.KeyCache, "Received unused #%d for (%q / %q)", seq, base.UD(docID), syncData.CurrentRev)
		change := &LogEntry{
			Sequence:     seq,
			TimeReceived: time.Now(),
		}
		changedChannels := c.processEntry(change)
		changedChannelsCombined = changedChannelsCombined.Update(changedChannels)
	}

	// If the recent sequence history includes any sequences earlier than the current sequence, and
	// not already seen by the gateway (more recent than c.nextSequence), add them as empty entries
	// so that they are included in sequence buffering.
	// If one of these sequences represents a removal from a channel then set the LogEntry removed flag
	// and the set of channels it was removed from
	currentSequence := syncData.Sequence
	if len(syncData.UnusedSequences) > 0 {
		currentSequence = syncData.UnusedSequences[0]
	}

	if len(syncData.RecentSequences) > 0 {

		for _, seq := range syncData.RecentSequences {
			if seq >= c.getNextSequence() && seq < currentSequence {
				base.Infof(base.KeyCache, "Received deduplicated #%d for (%q / %q)", seq, base.UD(docID), syncData.CurrentRev)
				change := &LogEntry{
					Sequence:     seq,
					TimeReceived: time.Now(),
				}

				//if the doc was removed from one or more channels at this sequence
				// Set the removed flag and removed channel set on the LogEntry
				if channelRemovals, atRevId := syncData.Channels.ChannelsRemovedAtSequence(seq); len(channelRemovals) > 0 {
					change.DocID = docID
					change.RevID = atRevId
					change.Channels = channelRemovals
				}

				changedChannels := c.processEntry(change)
				changedChannelsCombined = changedChannelsCombined.Update(changedChannels)
			}
		}
	}

	// Now add the entry for the new doc revision:
	change := &LogEntry{
		Sequence:     syncData.Sequence,
		DocID:        docID,
		RevID:        syncData.CurrentRev,
		Flags:        syncData.Flags,
		TimeReceived: time.Now(),
		TimeSaved:    syncData.TimeSaved,
		Channels:     syncData.Channels,
	}
	base.Infof(base.KeyCache, "Received #%d after %3dms (%q / %q)", change.Sequence, int(tapLag/time.Millisecond), base.UD(change.DocID), change.RevID)

	changedChannels := c.processEntry(change)
	changedChannelsCombined = changedChannelsCombined.Update(changedChannels)

	// Notify change listeners for all of the changed channels
	if c.notifyChange != nil && len(changedChannelsCombined) > 0 {
		c.notifyChange(changedChannelsCombined)
	}

}

// Remove purges the given doc IDs from all channel caches and returns the number of items removed.
// count will be larger than the input slice if the same document is removed from multiple channel caches.
func (c *changeCache) Remove(docIDs []string, startTime time.Time) (count int) {
	// Exit early if there's no work to do
	if len(docIDs) == 0 {
		return 0
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	for channelName := range c.channelCaches {
		count += c._getChannelCache(channelName).Remove(docIDs, startTime)
	}

	return count
}

func (c *changeCache) unmarshalPrincipal(docJSON []byte, isUser bool) (auth.Principal, error) {

	c.context.BucketLock.RLock()
	defer c.context.BucketLock.RUnlock()
	if c.context.Bucket != nil {
		return c.context.Authenticator().UnmarshalPrincipal(docJSON, "", 0, isUser)
	} else {
		return nil, fmt.Errorf("Attempt to unmarshal principal using closed bucket")
	}
}

// Process unused sequence notification.  Extracts sequence from docID and sends to cache for buffering
func (c *changeCache) processUnusedSequence(docID string) {
	sequenceStr := strings.TrimPrefix(docID, UnusedSequenceKeyPrefix)
	sequence, err := strconv.ParseUint(sequenceStr, 10, 64)
	if err != nil {
		base.Warnf(base.KeyAll, "Unable to identify sequence number for unused sequence notification with key: %s, error:", base.UD(docID), err)
		return
	}
	c.releaseUnusedSequence(sequence)

}

func (c *changeCache) releaseUnusedSequence(sequence uint64) {
	change := &LogEntry{
		Sequence:     sequence,
		TimeReceived: time.Now(),
	}
	base.Infof(base.KeyCache, "Received #%d (unused sequence)", sequence)

	// Since processEntry may unblock pending sequences, if there were any changed channels we need
	// to notify any change listeners that are working changes feeds for these channels
	changedChannels := c.processEntry(change)
	if c.notifyChange != nil && len(changedChannels) > 0 {
		c.notifyChange(changedChannels)
	}
}

// Process unused sequence notification.  Extracts sequence from docID and sends to cache for buffering
func (c *changeCache) processUnusedSequenceRange(docID string) {
	// _sync:unusedSequences:fromSeq:toSeq
	sequences := strings.Split(docID, ":")
	if len(sequences) != 4 {
		return
	}

	fromSequence, err := strconv.ParseUint(sequences[2], 10, 64)
	if err != nil {
		base.Warnf(base.KeyAll, "Unable to identify from sequence number for unused sequences notification with key: %s, error:", base.UD(docID), err)
		return
	}
	toSequence, err := strconv.ParseUint(sequences[3], 10, 64)
	if err != nil {
		base.Warnf(base.KeyAll, "Unable to identify to sequence number for unused sequence notification with key: %s, error:", base.UD(docID), err)
		return
	}

	// TODO: There should be a more efficient way to do this
	for seq := fromSequence; seq <= toSequence; seq++ {
		c.releaseUnusedSequence(seq)
	}
}

func (c *changeCache) processPrincipalDoc(docID string, docJSON []byte, isUser bool) {
	// Currently the cache isn't really doing much with user docs; mostly it needs to know about
	// them because they have sequence numbers, so without them the sequence of sequences would
	// have gaps in it, causing later sequences to get stuck in the queue.
	princ, err := c.unmarshalPrincipal(docJSON, isUser)
	if princ == nil {
		base.Warnf(base.KeyAll, "changeCache: Error unmarshaling doc %q: %v", base.UD(docID), err)
		return
	}
	sequence := princ.Sequence()

	if sequence <= c.getInitialSequence() {
		return // Tap is sending us an old value from before I started up; ignore it
	}

	// Now add the (somewhat fictitious) entry:
	change := &LogEntry{
		Sequence:     sequence,
		TimeReceived: time.Now(),
	}
	if isUser {
		change.DocID = "_user/" + princ.Name()
	} else {
		change.DocID = "_role/" + princ.Name()
	}

	base.Infof(base.KeyCache, "Received #%d (%q)", change.Sequence, base.UD(change.DocID))

	changedChannels := c.processEntry(change)
	if c.notifyChange != nil && len(changedChannels) > 0 {
		c.notifyChange(changedChannels)
	}
}

// Handles a newly-arrived LogEntry.
func (c *changeCache) processEntry(change *LogEntry) base.Set {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.logsDisabled {
		return nil
	}

	sequence := change.Sequence

	if _, found := c.receivedSeqs[sequence]; found {
		base.Debugf(base.KeyCache, "  Ignoring duplicate of #%d", sequence)
		return nil
	}
	c.receivedSeqs[sequence] = struct{}{}
	// FIX: c.receivedSeqs grows monotonically. Need a way to remove old sequences.

	var changedChannels base.Set
	if sequence == c.nextSequence || c.nextSequence == 0 {
		// This is the expected next sequence so we can add it now:
		changedChannels = c._addToCache(change)
		// Also add any pending sequences that are now contiguous:
		changedChannels = changedChannels.Update(c._addPendingLogs())
	} else if sequence > c.nextSequence {
		// There's a missing sequence (or several), so put this one on ice until it arrives:
		heap.Push(&c.pendingLogs, change)
		numPending := len(c.pendingLogs)
		base.Infof(base.KeyCache, "  Deferring #%d (%d now waiting for #%d...#%d)",
			sequence, numPending, c.nextSequence, c.pendingLogs[0].Sequence-1)
		changeCacheExpvars.Get("maxPending").(*base.IntMax).SetIfMax(int64(numPending))
		if numPending > c.options.CachePendingSeqMaxNum {
			// Too many pending; add the oldest one:
			changedChannels = c._addPendingLogs()
		}
	} else if sequence > c.initialSequence {
		// Out-of-order sequence received!
		// Remove from skipped sequence queue
		if !c.WasSkipped(sequence) {
			// Error removing from skipped sequences
			base.Infof(base.KeyCache, "  Received unexpected out-of-order change - not in skippedSeqs (seq %d, expecting %d) doc %q / %q", sequence, c.nextSequence, base.UD(change.DocID), change.RevID)
		} else {
			base.Infof(base.KeyCache, "  Received previously skipped out-of-order change (seq %d, expecting %d) doc %q / %q ", sequence, c.nextSequence, base.UD(change.DocID), change.RevID)
			change.Skipped = true
		}

		changedChannels = c._addToCache(change)
		// Add to cache before removing from skipped, to ensure lowSequence doesn't get incremented until results are available
		// in cache
		c.RemoveSkipped(sequence)
	}
	return changedChannels
}

// Adds an entry to the appropriate channels' caches, returning the affected channels.  lateSequence
// flag indicates whether it was a change arriving out of sequence
func (c *changeCache) _addToCache(change *LogEntry) base.Set {

	if change.Sequence >= c.nextSequence {
		c.nextSequence = change.Sequence + 1
	}
	if change.DocID == "" {
		return nil // this was a placeholder for an unused sequence
	}
	addedTo := make(base.Set)
	ch := change.Channels
	change.Channels = nil // not needed anymore, so free some memory

	// If it's a late sequence, we want to add to all channel late queues within a single write lock,
	// to avoid a changes feed seeing the same late sequence in different iteration loops (and sending
	// twice)
	func() {
		if change.Skipped {
			c.lateSeqLock.Lock()
			base.Infof(base.KeyChanges, "Acquired late sequence lock for %d", change.Sequence)
			defer c.lateSeqLock.Unlock()
		}

		for channelName, removal := range ch {
			if removal == nil || removal.Seq == change.Sequence {
				channelCache := c._getChannelCache(channelName)
				channelCache.addToCache(change, removal != nil)
				addedTo = addedTo.Add(channelName)
				if change.Skipped {
					channelCache.AddLateSequence(change)
				}
			}
		}

		if EnableStarChannelLog {
			channelCache := c._getChannelCache(channels.UserStarChannel)
			channelCache.addToCache(change, false)
			addedTo = addedTo.Add(channels.UserStarChannel)
			if change.Skipped {
				channelCache.AddLateSequence(change)
			}
		}
	}()

	return addedTo
}

// Add the first change(s) from pendingLogs if they're the next sequence.  If not, and we've been
// waiting too long for nextSequence, move nextSequence to skipped queue.
// Returns the channels that changed.
func (c *changeCache) _addPendingLogs() base.Set {
	var changedChannels base.Set

	for len(c.pendingLogs) > 0 {
		change := c.pendingLogs[0]
		isNext := change.Sequence == c.nextSequence
		if isNext {
			heap.Pop(&c.pendingLogs)
			changedChannels = changedChannels.Update(c._addToCache(change))
		} else if len(c.pendingLogs) > c.options.CachePendingSeqMaxNum || time.Since(c.pendingLogs[0].TimeReceived) >= c.options.CachePendingSeqMaxWait {
			changeCacheExpvars.Add("outOfOrder", 1)
			c.PushSkipped(c.nextSequence)
			c.nextSequence++
		} else {
			break
		}
	}
	return changedChannels
}

func (c *changeCache) getChannelCache(channelName string) *channelCache {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c._getChannelCache(channelName)
}

func (c *changeCache) GetStableSequence(docID string) SequenceID {
	// Stable sequence is independent of docID in changeCache
	return SequenceID{Seq: c.LastSequence()}
}

func (c *changeCache) GetStableClock(stale bool) (clock base.SequenceClock, err error) {
	return nil, errors.New("Change cache doesn't use vector clocks")
}

func (c *changeCache) _getChannelCache(channelName string) *channelCache {
	cache := c.channelCaches[channelName]
	if cache == nil {

		// expect to see everything _after_ the sequence at the time of cache init, but not the sequence itself since it not expected to appear on DCP
		validFrom := c.initialSequence + 1

		cache = newChannelCacheWithOptions(c.context, channelName, validFrom, c.options)
		c.channelCaches[channelName] = cache
	}
	return cache
}

//////// CHANGE ACCESS:

func (c *changeCache) GetChanges(channelName string, options ChangesOptions) ([]*LogEntry, error) {

	if c.IsStopped() {
		return nil, base.HTTPErrorf(503, "Database closed")
	}
	return c.getChannelCache(channelName).GetChanges(options)
}

func (c *changeCache) GetCachedChanges(channelName string, options ChangesOptions) (uint64, []*LogEntry) {
	return c.getChannelCache(channelName).getCachedChanges(options)
}

// Returns the sequence number the cache is up-to-date with.
func (c *changeCache) LastSequence() uint64 {

	lastSequence := c.getNextSequence() - 1
	return lastSequence
}

func (c *changeCache) _allChannels() base.Set {
	allChannelSet := make(base.Set)
	for name := range c.channelCaches {
		allChannelSet = allChannelSet.Add(name)
	}
	return allChannelSet
}

func (c *changeCache) getOldestSkippedSequence() uint64 {
	c.skippedSeqLock.RLock()
	defer c.skippedSeqLock.RUnlock()
	return c.skippedSeqs.getOldest()
}

// Set the initial sequence.  Presumes that change chache is already locked.
func (c *changeCache) _setInitialSequence(initialSequence uint64) {
	c.initialSequence = initialSequence
	c.nextSequence = initialSequence + 1
}

// Concurrent-safe get value of nextSequence
func (c *changeCache) getNextSequence() uint64 {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.nextSequence
}

// Concurrent-safe get value of initialSequence
func (c *changeCache) getInitialSequence() uint64 {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.initialSequence
}

//////// LOG PRIORITY QUEUE -- container/heap callbacks that should not be called directly.   Use heap.Init/Push/etc()

func (h LogPriorityQueue) Len() int           { return len(h) }
func (h LogPriorityQueue) Less(i, j int) bool { return h[i].Sequence < h[j].Sequence }
func (h LogPriorityQueue) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *LogPriorityQueue) Push(x interface{}) {
	*h = append(*h, x.(*LogEntry))
}

func (h *LogPriorityQueue) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

//////// SKIPPED SEQUENCE QUEUE

func (c *changeCache) RemoveSkipped(x uint64) error {
	c.skippedSeqLock.Lock()
	defer c.skippedSeqLock.Unlock()
	return c.skippedSeqs.Remove(x)
}

// Removes a set of sequences.  Logs warning on removal error, returns count of successfully removed.
func (c *changeCache) RemoveSkippedSequences(sequences []uint64) (removedCount int64) {
	c.skippedSeqLock.Lock()
	defer c.skippedSeqLock.Unlock()
	for _, seq := range sequences {
		err := c.skippedSeqs.Remove(seq)
		if err != nil {
			base.Warnf(base.KeyAll, "Error purging skipped sequence %d from skipped sequence queue: %v", seq, err)
		} else {
			removedCount++
		}
	}
	return removedCount
}

func (c *changeCache) WasSkipped(x uint64) bool {
	c.skippedSeqLock.RLock()
	defer c.skippedSeqLock.RUnlock()

	return c.skippedSeqs.Contains(x)
}

func (c *changeCache) PushSkipped(sequence uint64) {
	c.skippedSeqLock.Lock()
	defer c.skippedSeqLock.Unlock()
	c.skippedSeqs.Push(&SkippedSequence{seq: sequence, timeAdded: time.Now()})
}

func (c *changeCache) GetSkippedSequencesOlderThanMaxWait() (oldSequences []uint64) {
	c.skippedSeqLock.RLock()
	defer c.skippedSeqLock.RUnlock()

	return c.skippedSeqs.getOlderThan(c.options.CacheSkippedSeqMaxWait)
}

// SkippedSequenceList stores the set of skipped sequences as an ordered list of *SkippedSequence with an associated map
// for sequence-based lookup.
type SkippedSequenceList struct {
	skippedList *list.List
	skippedMap  map[uint64]*list.Element
}

func NewSkippedSequenceList() *SkippedSequenceList {

	return &SkippedSequenceList{
		skippedMap:  map[uint64]*list.Element{},
		skippedList: list.New(),
	}
}

// getOldest returns the sequence of the first element in the skippedSequenceList
func (l *SkippedSequenceList) getOldest() uint64 {
	if firstElement := l.skippedList.Front(); firstElement != nil {
		value := firstElement.Value.(*SkippedSequence)
		base.Debugf(base.KeyChanges, "Get oldest skipped, returning: %d", value.seq)
		return value.seq
	} else {
		return uint64(0)
	}
}

// Remove does a simple binary search to find and remove.
func (l *SkippedSequenceList) Remove(x uint64) error {

	if listElement, ok := l.skippedMap[x]; ok {
		l.skippedList.Remove(listElement)
		delete(l.skippedMap, x)
		return nil
	} else {
		return errors.New("Value not found")
	}
}

// Contains does a simple search to detect presence
func (l *SkippedSequenceList) Contains(x uint64) bool {
	_, ok := l.skippedMap[x]
	return ok
}

func (l *SkippedSequenceList) Push(x *SkippedSequence) error {

	validPush := false
	lastElement := l.skippedList.Back()
	if lastElement == nil {
		validPush = true
	} else {
		lastSkipped, _ := lastElement.Value.(*SkippedSequence)
		if lastSkipped.seq < x.seq {
			validPush = true
		}
	}
	if validPush {
		newListElement := l.skippedList.PushBack(x)
		l.skippedMap[x.seq] = newListElement
		return nil
	} else {
		return errors.New("Can't push sequence lower than existing maximum")
	}

}

// getOldest returns a slice of sequences older than the specified duration of the first element in the skippedSequenceList
func (l *SkippedSequenceList) getOlderThan(skippedExpiry time.Duration) []uint64 {
	oldSequences := make([]uint64, 0)
	for e := l.skippedList.Front(); e != nil; e = e.Next() {
		skippedSeq := e.Value.(*SkippedSequence)
		if time.Since(skippedSeq.timeAdded) > skippedExpiry {
			oldSequences = append(oldSequences, skippedSeq.seq)
		} else {
			// skippedSeqs are ordered by arrival time, so can stop iterating once we find one
			// still inside the time window
			break
		}
	}
	return oldSequences
}
