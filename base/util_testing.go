/*
Copyright 2017-Present Couchbase, Inc.

Use of this software is governed by the Business Source License included in
the file licenses/BSL-Couchbase.txt.  As of the Change Date specified in that
file, in accordance with the Business Source License, use of this software will
be governed by the Apache License, Version 2.0, included in the file
licenses/APL2.txt.
*/

package base

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sgbucket "github.com/couchbase/sg-bucket"
	"github.com/stretchr/testify/require"
	"gopkg.in/couchbase/gocb.v1"

	"github.com/stretchr/testify/assert"
)

// Code that is test-related that needs to be accessible from non-base packages, and therefore can't live in
// util_test.go, which is only accessible from the base package.

var TestExternalRevStorage = false
var numOpenBucketsByName map[string]int32
var mutexNumOpenBucketsByName sync.Mutex

func init() {

	// Prevent https://issues.couchbase.com/browse/MB-24237
	rand.Seed(time.Now().UTC().UnixNano())

	numOpenBucketsByName = map[string]int32{}

}

type TestBucket struct {
	Bucket
	BucketSpec BucketSpec
	closeFn    func()
}

func (tb TestBucket) Close() {
	tb.closeFn()
}

// LeakyBucketClone wraps the underlying bucket on the TestBucket with a LeakyBucket and returns a new TestBucket handle.
func (tb *TestBucket) LeakyBucketClone(c LeakyBucketConfig) *TestBucket {
	return &TestBucket{
		Bucket:     NewLeakyBucket(tb.Bucket, c),
		BucketSpec: tb.BucketSpec,
		closeFn:    tb.Close,
	}
}

// NoCloseClone returns a leaky bucket with a no-op close function for the given bucket.
func NoCloseClone(b Bucket) *LeakyBucket {
	return NewLeakyBucket(b, LeakyBucketConfig{IgnoreClose: true})
}

// NoCloseClone returns a new test bucket referencing the same underlying bucket and bucketspec, but
// with an IgnoreClose leaky bucket, and a no-op close function.  Used when multiple references to the same bucket are needed.
func (tb *TestBucket) NoCloseClone() *TestBucket {
	return &TestBucket{
		Bucket:     NoCloseClone(tb.Bucket),
		BucketSpec: tb.BucketSpec,
		closeFn:    func() {},
	}
}

func GetTestBucket(t testing.TB) *TestBucket {
	bucket, spec, closeFn := GTestBucketPool.GetTestBucketAndSpec(t)
	return &TestBucket{
		Bucket:     bucket,
		BucketSpec: spec,
		closeFn:    closeFn,
	}
}

// Gets a Walrus bucket which will be persisted to a temporary directory
// Returns both the test bucket which is persisted and a function which can be used to remove the created temporary
// directory once the test has finished with it.
func GetPersistentWalrusBucket(t testing.TB) (*TestBucket, func()) {
	tempDir, err := ioutil.TempDir("", "walrustemp")
	require.NoError(t, err)

	walrusFile := fmt.Sprintf("walrus:%s", tempDir)
	bucket, spec, closeFn := GTestBucketPool.GetWalrusTestBucket(t, walrusFile)

	// Return this separate to closeFn as we want to avoid this being removed on database close (/_offline handling)
	removeFileFunc := func() {
		err := os.RemoveAll(tempDir)
		require.NoError(t, err)
	}

	return &TestBucket{
		Bucket:     bucket,
		BucketSpec: spec,
		closeFn:    closeFn,
	}, removeFileFunc
}

func GetTestBucketForDriver(t testing.TB, driver CouchbaseDriver) *TestBucket {
	if driver == GoCBv2 {
		// TODO: add GoCBv2 support to TestBucketPool.

		// Reserve test bucket from pool
		_, spec, closeFn := GTestBucketPool.GetTestBucketAndSpec(t)

		spec.CouchbaseDriver = GoCBv2
		if spec.Server == kTestCouchbaseServerURL {
			spec.Server = "couchbase://localhost"
		}
		if !strings.HasPrefix(spec.Server, "couchbase") {
			closeFn()
			t.Fatalf("Server must use couchbase scheme for gocb v2 testing")
		}

		collection, err := GetCouchbaseCollection(spec)
		if err != nil {
			t.Fatalf("Unable to get collection: %v", collection)
		}

		closeAll := func() {
			collection.Close()
			closeFn()
		}

		return &TestBucket{
			Bucket:     collection,
			BucketSpec: spec,
			closeFn:    closeAll,
		}

	} else {
		return GetTestBucket(t)
	}
}

// Should Sync Gateway use XATTRS functionality when running unit tests?
func TestUseXattrs() bool {

	// First check if the SG_TEST_USE_XATTRS env variable is set
	useXattrs := os.Getenv(TestEnvSyncGatewayUseXattrs)
	if strings.ToLower(useXattrs) == strings.ToLower(TestEnvSyncGatewayTrue) {
		return true
	}

	// Otherwise fallback to hardcoded default
	return DefaultUseXattrs

}

// Should tests try to drop GSI indexes before flushing buckets?
// See SG #3422
func TestsShouldDropIndexes() bool {

	// First check if the SG_TEST_USE_XATTRS env variable is set
	dropIndexes := os.Getenv(TestEnvSyncGatewayDropIndexes)

	if strings.ToLower(dropIndexes) == strings.ToLower(TestEnvSyncGatewayTrue) {
		return true
	}

	// Otherwise fallback to hardcoded default
	return DefaultDropIndexes

}

// TestsDisableGSI returns true if tests should be forced to avoid any GSI-specific code.
func TestsDisableGSI() bool {
	// Disable GSI when running with Walrus
	if !TestUseCouchbaseServer() && UnitTestUrlIsWalrus() {
		return true
	}

	// Default to disabling GSI, but allow with SG_TEST_USE_GSI=true
	useGSI := false
	if envUseGSI := os.Getenv(TestEnvSyncGatewayDisableGSI); envUseGSI != "" {
		useGSI, _ = strconv.ParseBool(envUseGSI)
	}

	return !useGSI
}

// Check the whether tests are being run with SG_TEST_BACKING_STORE=Couchbase
func TestUseCouchbaseServer() bool {
	backingStore := os.Getenv(TestEnvSyncGatewayBackingStore)
	return strings.ToLower(backingStore) == strings.ToLower(TestEnvBackingStoreCouchbase)
}

type TestAuthenticator struct {
	Username   string
	Password   string
	BucketName string
}

func (t TestAuthenticator) GetCredentials() (username, password, bucketname string) {
	return t.Username, t.Password, t.BucketName
}

// Reset bucket state
func DropAllBucketIndexes(gocbBucket *CouchbaseBucketGoCB) error {

	// Retrieve all indexes
	indexes, err := getIndexes(gocbBucket)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	wg.Add(len(indexes))

	asyncErrors := make(chan error, len(indexes))
	defer close(asyncErrors)

	for _, index := range indexes {

		go func(indexToDrop string) {

			defer wg.Done()

			log.Printf("Dropping index %s on bucket %s...", indexToDrop, gocbBucket.Name())
			dropErr := gocbBucket.DropIndex(indexToDrop)
			if dropErr != nil {
				asyncErrors <- dropErr
				log.Printf("...failed to drop index %s on bucket %s: %s", indexToDrop, gocbBucket.Name(), dropErr)
				return
			}
			log.Printf("...successfully dropped index %s on bucket %s", indexToDrop, gocbBucket.Name())
		}(index)

	}

	// Wait until all goroutines finish
	wg.Wait()

	// Check if any errors were put into the asyncErrors channel.  If any, just return the first one
	select {
	case asyncError := <-asyncErrors:
		return asyncError
	default:
	}

	return nil
}

// Get a list of all index names in the bucket
func getIndexes(gocbBucket *CouchbaseBucketGoCB) (indexes []string, err error) {

	indexes = []string{}

	manager, err := gocbBucket.getBucketManager()
	if err != nil {
		return indexes, err
	}

	indexInfo, err := manager.GetIndexes()
	if err != nil {
		return indexes, err
	}

	for _, indexInfo := range indexInfo {
		if indexInfo.Keyspace == gocbBucket.GetName() {
			indexes = append(indexes, indexInfo.Name)
		}
	}

	return indexes, nil
}

// Generates a string of size int
const alphaNumeric = "0123456789abcdefghijklmnopqrstuvwxyz"

func CreateProperty(size int) (result string) {
	resultBytes := make([]byte, size)
	for i := 0; i < size; i++ {
		resultBytes[i] = alphaNumeric[i%len(alphaNumeric)]
	}
	return string(resultBytes)
}

var GlobalTestLoggingSet = AtomicBool{}

// SetUpGlobalTestLogging sets a global log level at runtime by using the SG_TEST_LOG_LEVEL environment variable.
// This global level overrides any tests that specify their own test log level with SetUpTestLogging.
func SetUpGlobalTestLogging(m *testing.M) (teardownFn func()) {
	if logLevel := os.Getenv(TestEnvGlobalLogLevel); logLevel != "" {
		var l LogLevel
		err := l.UnmarshalText([]byte(logLevel))
		if err != nil {
			Fatalf("Invalid log level used for %q: %s", TestEnvGlobalLogLevel, err)
		}
		teardown := SetUpTestLogging(l, KeyAll)
		GlobalTestLoggingSet.Set(true)
		return func() {
			teardown()
			GlobalTestLoggingSet.Set(false)
		}
	}
	// noop
	return func() { GlobalTestLoggingSet.Set(false) }
}

// SetUpTestLogging will set the given log level and log keys,
// and return a function that can be deferred for teardown.
//
// This function will panic if called multiple times without running the teardownFn.
//
// To set multiple log keys, append as variadic arguments
// E.g. KeyCache,KeyDCP,KeySync
//
// Usage:
//     teardownFn := SetUpTestLogging(LevelDebug, KeyCache,KeyDCP,KeySync)
//     defer teardownFn()
//
// Shorthand style:
//     defer SetUpTestLogging(LevelDebug, KeyCache,KeyDCP,KeySync)()
func SetUpTestLogging(logLevel LogLevel, logKeys ...LogKey) (teardownFn func()) {
	caller := GetCallersName(1, false)
	Infof(KeyAll, "%s: Setup logging: level: %v - keys: %v", caller, logLevel, logKeys)
	return setTestLogging(logLevel, caller, logKeys...)
}

// DisableTestLogging is an alias for SetUpTestLogging(LevelNone, KeyNone)
// This function will panic if called multiple times without running the teardownFn.
func DisableTestLogging() (teardownFn func()) {
	caller := ""
	return setTestLogging(LevelNone, caller, KeyNone)
}

// SetUpBenchmarkLogging will set the given log level and key, and do log processing for that configuration,
// but discards the output, instead of writing it to console.
func SetUpBenchmarkLogging(logLevel LogLevel, logKeys ...LogKey) (teardownFn func()) {
	teardownFnOrig := setTestLogging(logLevel, "", logKeys...)

	// discard all logging output for benchmarking (but still execute logging as normal)
	consoleLogger.logger.SetOutput(ioutil.Discard)
	return func() {
		// revert back to original output
		if consoleLogger != nil && consoleLogger.output != nil {
			consoleLogger.logger.SetOutput(consoleLogger.output)
		} else {
			consoleLogger.logger.SetOutput(os.Stderr)
		}
		teardownFnOrig()
	}
}

func setTestLogging(logLevel LogLevel, caller string, logKeys ...LogKey) (teardownFn func()) {
	if GlobalTestLoggingSet.IsTrue() {
		// noop, test log level is already set globally
		return func() {
			if caller != "" {
				Infof(KeyAll, "%v: Reset logging", caller)
			}
		}
	}

	initialLogLevel := LevelInfo
	initialLogKey := logKeyMask(KeyHTTP)

	// Check that a previous invocation has not forgotten to call teardownFn
	if *consoleLogger.LogLevel != initialLogLevel ||
		*consoleLogger.LogKeyMask != *initialLogKey {
		panic("Logging is in an unexpected state! Did a previous test forget to call the teardownFn of SetUpTestLogging?")
	}

	consoleLogger.LogLevel.Set(logLevel)
	consoleLogger.LogKeyMask.Set(logKeyMask(logKeys...))

	return func() {
		// Return logging to a default state
		consoleLogger.LogLevel.Set(initialLogLevel)
		consoleLogger.LogKeyMask.Set(initialLogKey)
		if caller != "" {
			Infof(KeyAll, "%v: Reset logging", caller)
		}
	}
}

// Make a deep copy from src into dst.
// Copied from https://github.com/getlantern/deepcopy, commit 7f45deb8130a0acc553242eb0e009e3f6f3d9ce3 (Apache 2 licensed)
func DeepCopyInefficient(dst interface{}, src interface{}) error {
	if dst == nil {
		return fmt.Errorf("dst cannot be nil")
	}
	if src == nil {
		return fmt.Errorf("src cannot be nil")
	}
	b, err := JSONMarshal(src)
	if err != nil {
		return fmt.Errorf("Unable to marshal src: %s", err)
	}
	d := JSONDecoder(bytes.NewBuffer(b))
	d.UseNumber()
	err = d.Decode(dst)
	if err != nil {
		return fmt.Errorf("Unable to unmarshal into dst: %s", err)
	}
	return nil
}

// testRetryUntilTrue performs a short sleep-based retry loop until the timeout is reached or the
// criteria in RetryUntilTrueFunc is met. Intended to
// avoid arbitrarily long sleeps in tests that don't have any alternative to polling.
// Default sleep time is 50ms, timeout is 10s.  Can be customized with testRetryUntilTrueCustom
type RetryUntilTrueFunc func() bool

func testRetryUntilTrue(t *testing.T, retryFunc RetryUntilTrueFunc) {
	testRetryUntilTrueCustom(t, retryFunc, 100, 10000)
}

func testRetryUntilTrueCustom(t *testing.T, retryFunc RetryUntilTrueFunc, waitTimeMs int, timeoutMs int) {
	timeElapsedMs := 0
	for timeElapsedMs < timeoutMs {
		if retryFunc() {
			return
		}
		time.Sleep(time.Duration(waitTimeMs) * time.Millisecond)
		timeElapsedMs += waitTimeMs
	}
	assert.Fail(t, fmt.Sprintf("Retry until function didn't succeed within timeout (%d ms)", timeoutMs))
}

func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func DirExists(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// WaitForStat will retry for up to 20 seconds until the result of getStatFunc is equal to the expected value.
func WaitForStat(getStatFunc func() int64, expected int64) (int64, bool) {
	workerFunc := func() (shouldRetry bool, err error, val interface{}) {
		val = getStatFunc()
		return val != expected, nil, val
	}
	// wait for up to 20 seconds for the stat to meet the expected value
	err, val := RetryLoop("waitForStat retry loop", workerFunc, CreateSleeperFunc(200, 100))
	valInt64, ok := val.(int64)

	return valInt64, err == nil && ok
}

func WriteXattr(gocbBucket *CouchbaseBucketGoCB, docKey string, xattrKey string, xattrVal interface{}) (uint64, error) {
	docFrag, err := gocbBucket.Bucket.MutateIn(docKey, 0, 0).UpsertEx(xattrKey, xattrVal, gocb.SubdocFlagXattr|gocb.SubdocFlagCreatePath).Execute()
	if err != nil {
		return 0, err
	}

	return uint64(docFrag.Cas()), nil
}

func DeleteXattr(gocbBucket *CouchbaseBucketGoCB, docKey string, xattrKey string) (uint64, error) {
	docFrag, err := gocbBucket.Bucket.MutateIn(docKey, 0, 0).RemoveEx(xattrKey, gocb.SubdocFlagXattr).Execute()
	if err != nil {
		return 0, err
	}

	return uint64(docFrag.Cas()), nil
}

type dataStore struct {
	name   string
	driver CouchbaseDriver
}

// ForAllDataStores is used to run a test against multiple data stores (gocb bucket, gocb collection)
func ForAllDataStores(t *testing.T, testCallback func(*testing.T, sgbucket.DataStore)) {
	dataStores := make([]dataStore, 0)

	if TestUseCouchbaseServer() {
		dataStores = append(dataStores, dataStore{
			name:   "gocb.v2",
			driver: GoCBv2,
		})
	}
	dataStores = append(dataStores, dataStore{
		name:   "gocb.v1",
		driver: GoCB,
	})

	for _, dataStore := range dataStores {
		t.Run(dataStore.name, func(t *testing.T) {
			bucket := GetTestBucketForDriver(t, dataStore.driver)
			defer bucket.Close()
			testCallback(t, bucket)
		})
	}
}
