package base

import (
	"encoding/json"
	"errors"

	"github.com/couchbase/gocb"
	"github.com/couchbase/gocbcore"
	"github.com/couchbase/gocbcore/memd"
	sgbucket "github.com/couchbase/sg-bucket"
	pkgerrors "github.com/pkg/errors"
)

var GetSpecXattr = &gocb.GetSpecOptions{IsXattr: true}
var InsertSpecXattr = &gocb.InsertSpecOptions{IsXattr: true}
var UpsertSpecXattr = &gocb.UpsertSpecOptions{IsXattr: true}
var RemoveSpecXattr = &gocb.RemoveSpecOptions{IsXattr: true}
var LookupOptsAccessDeleted *gocb.LookupInOptions

var _ SubdocXattrStore = &Collection{}

func init() {
	LookupOptsAccessDeleted = &gocb.LookupInOptions{}
	LookupOptsAccessDeleted.Internal.DocFlags = gocb.SubdocDocFlagAccessDeleted
}

func (c *Collection) GetSpec() BucketSpec {
	return c.Spec
}

// Implementation of the XattrStore interface primarily invokes common wrappers that in turn invoke SDK-specific SubdocXattrStore API
func (c *Collection) WriteCasWithXattr(k string, xattrKey string, exp uint32, cas uint64, v interface{}, xv interface{}) (casOut uint64, err error) {
	return WriteCasWithXattr(c, k, xattrKey, exp, cas, v, xv)
}

func (c *Collection) WriteWithXattr(k string, xattrKey string, exp uint32, cas uint64, v []byte, xv []byte, isDelete bool, deleteBody bool) (casOut uint64, err error) { // If this is a tombstone, we want to delete the document and update the xattr
	return WriteWithXattr(c, k, xattrKey, exp, cas, v, xv, isDelete, deleteBody)
}

func (c *Collection) DeleteWithXattr(k string, xattrKey string) error {
	return DeleteWithXattr(c, k, xattrKey)
}

func (c *Collection) GetXattr(k string, xattrKey string, xv interface{}) (casOut uint64, err error) {
	return c.SubdocGetXattr(k, xattrKey, xv)
}

func (c *Collection) GetWithXattr(k string, xattrKey string, userXattrKey string, rv interface{}, xv interface{}, uxv interface{}) (cas uint64, err error) {
	return c.SubdocGetBodyAndXattr(k, xattrKey, userXattrKey, rv, xv, uxv)
}

func (c *Collection) WriteUpdateWithXattr(k string, xattrKey string, userXattrKey string, exp uint32, previous *sgbucket.BucketDocument, callback sgbucket.WriteUpdateWithXattrFunc) (casOut uint64, err error) {
	return WriteUpdateWithXattr(c, k, xattrKey, userXattrKey, exp, previous, callback)
}

func (c *Collection) UpdateXattr(k string, xattrKey string, exp uint32, cas uint64, xv interface{}, deleteBody bool, isDelete bool) (casOut uint64, err error) {
	return UpdateTombstoneXattr(c, k, xattrKey, exp, cas, xv, deleteBody)
}

// SubdocGetXattr retrieves the named xattr
func (c *Collection) SubdocGetXattr(k string, xattrKey string, xv interface{}) (casOut uint64, err error) {

	ops := []gocb.LookupInSpec{
		gocb.GetSpec(xattrKey, GetSpecXattr),
	}
	res, lookupErr := c.LookupIn(k, ops, LookupOptsAccessDeleted)

	if lookupErr == nil {
		xattrContErr := res.ContentAt(0, xv)
		if xattrContErr != nil {
			Debugf(KeyCRUD, "No xattr content found for key=%s, xattrKey=%s: %v", UD(k), UD(xattrKey), xattrContErr)
			return 0, ErrXattrNotFound
		}
		cas := uint64(res.Cas())
		return cas, nil
	} else if isKVError(lookupErr, memd.StatusSubDocBadMulti) {
		xattrErr := res.ContentAt(0, xv)
		if xattrErr != nil {
			Debugf(KeyCRUD, "No xattr content found for key=%s, xattrKey=%s: %v", UD(k), UD(xattrKey), xattrErr)
			return 0, ErrXattrNotFound
		}
		cas := uint64(res.Cas())
		return cas, nil
	} else if isKVError(lookupErr, memd.StatusKeyNotFound) {
		Debugf(KeyCRUD, "No document found for key=%s", UD(k))
		return 0, ErrNotFound
	} else if isKVError(lookupErr, memd.StatusSubDocMultiPathFailureDeleted) || isKVError(lookupErr, memd.StatusSubDocSuccessDeleted) {
		xattrContentErr := res.ContentAt(0, xv)
		if xattrContentErr != nil {
			return 0, ErrNotFound
		}
		cas := uint64(res.Cas())
		return cas, nil
	} else {
		return 0, lookupErr
	}
}

// SubdocGetBodyAndXattr retrieves the document body and xattr in a single LookupIn subdoc operation.  Does not require both to exist.
func (c *Collection) SubdocGetBodyAndXattr(k string, xattrKey string, userXattrKey string, rv interface{}, xv interface{}, uxv interface{}) (cas uint64, err error) {
	worker := func() (shouldRetry bool, err error, value uint64) {

		// First, attempt to get the document and xattr in one shot.
		ops := []gocb.LookupInSpec{
			gocb.GetSpec(xattrKey, GetSpecXattr),
			gocb.GetSpec("", &gocb.GetSpecOptions{}),
		}
		res, lookupErr := c.LookupIn(k, ops, LookupOptsAccessDeleted)

		// There are two 'partial success' error codes:
		//   ErrSubDocBadMulti - one of the subdoc operations failed.  Occurs when doc exists but xattr does not
		//   ErrSubDocMultiPathFailureDeleted - one of the subdoc operations failed, and the doc is deleted.  Occurs when xattr exists but doc is deleted (tombstone)
		switch lookupErr {
		case nil, gocbcore.ErrMemdSubDocBadMulti:
			// Attempt to retrieve the document body, if present
			docContentErr := res.ContentAt(1, rv)
			xattrContentErr := res.ContentAt(0, xv)
			if isKVError(docContentErr, memd.StatusSubDocMultiPathFailureDeleted) && isKVError(xattrContentErr, memd.StatusSubDocMultiPathFailureDeleted) {
				// No doc, no xattr means the doc isn't found
				Debugf(KeyCRUD, "No xattr content found for key=%s, xattrKey=%s: %v", UD(k), UD(xattrKey), xattrContentErr)
				return false, ErrNotFound, cas
			}

			if docContentErr != nil {
				Debugf(KeyCRUD, "No document body found for key=%s, xattrKey=%s: %v", UD(k), UD(xattrKey), docContentErr)
			}
			// Attempt to retrieve the xattr, if present
			if xattrContentErr != nil {
				Debugf(KeyCRUD, "No xattr content found for key=%s, xattrKey=%s: %v", UD(k), UD(xattrKey), xattrContentErr)
			}
			cas = uint64(res.Cas())

		case gocbcore.ErrMemdSubDocMultiPathFailureDeleted:
			//   ErrSubDocMultiPathFailureDeleted - one of the subdoc operations failed, and the doc is deleted.  Occurs when xattr may exist but doc is deleted (tombstone)
			xattrContentErr := res.ContentAt(0, xv)
			cas = uint64(res.Cas())
			if xattrContentErr != nil {
				// No doc, no xattr means the doc isn't found
				Debugf(KeyCRUD, "No xattr content found for key=%s, xattrKey=%s: %v", UD(k), UD(xattrKey), xattrContentErr)
				return false, ErrNotFound, cas
			}
			return false, nil, cas
		default:
			// KeyNotFound is returned as KVError
			if isKVError(lookupErr, memd.StatusKeyNotFound) {
				return false, ErrNotFound, cas
			}
			shouldRetry = c.isRecoverableReadError(lookupErr)
			return shouldRetry, lookupErr, uint64(0)
		}

		// TODO: We may be able to improve in the future by having this secondary op as part of the first. At present
		// there is no support to obtain more than one xattr in a single operation however MB-28041 is filed for this.
		if userXattrKey != "" {
			userXattrCas, err := c.SubdocGetXattr(k, userXattrKey, uxv)
			switch pkgerrors.Cause(err) {
			case gocb.ErrDocumentNotFound:
				// If key not found it has been deleted in between the first op and this op.
				return false, err, userXattrCas
			case gocbcore.ErrMemdSubDocBadMulti:
				// Xattr doesn't exist, can skip
			case nil:
				if cas != userXattrCas {
					return true, errors.New("cas mismatch between user xattr and document body"), uint64(0)
				}
			default:
				// Unknown error occurred
				// Shouldn't retry as any recoverable error will have been retried already in SubdocGetXattr
				return false, err, uint64(0)
			}
		}
		return false, nil, cas
	}

	// Kick off retry loop
	err, cas = RetryLoopCas("SubdocGetBodyAndXattr", worker, c.Spec.RetrySleeper())
	if err != nil {
		err = pkgerrors.Wrapf(err, "SubdocGetBodyAndXattr %v", UD(k).Redact())
	}

	return cas, err
}

// SubdocInsertXattr inserts a new server tombstone with an associated mobile xattr.  Writes cas and crc32c to the xattr using
// macro expansion.
func (c *Collection) SubdocInsertXattr(k string, xattrKey string, exp uint32, cas uint64, xv interface{}) (casOut uint64, err error) {

	supportsTombstoneCreation := c.IsSupported(sgbucket.DataStoreFeatureCreateDeletedWithXattr)

	var docFlags gocb.SubdocDocFlag
	if supportsTombstoneCreation {
		docFlags = gocb.SubdocDocFlagCreateAsDeleted | gocb.SubdocDocFlagAccessDeleted | gocb.SubdocDocFlagAddDoc
	} else {
		docFlags = gocb.SubdocDocFlagMkDoc
	}

	mutateOps := []gocb.MutateInSpec{
		gocb.UpsertSpec(xattrKey, bytesToRawMessage(xv), UpsertSpecXattr),
		gocb.UpsertSpec(xattrCasPath(xattrKey), gocb.MutationMacroCAS, UpsertSpecXattr),
		gocb.UpsertSpec(xattrCrc32cPath(xattrKey), gocb.MutationMacroValueCRC32c, UpsertSpecXattr),
	}
	options := &gocb.MutateInOptions{
		StoreSemantic: gocb.StoreSemanticsUpsert,
		Expiry:        CbsExpiryToDuration(exp),
		Cas:           gocb.Cas(cas),
	}
	options.Internal.DocFlags = docFlags
	result, mutateErr := c.MutateIn(k, mutateOps, options)
	if mutateErr != nil {
		return 0, mutateErr
	}
	return uint64(result.Cas()), nil
}

// SubdocInsertXattr inserts a document and associated mobile xattr in a single mutateIn operation.  Writes cas and crc32c to the xattr using
// macro expansion.
func (c *Collection) SubdocInsertBodyAndXattr(k string, xattrKey string, exp uint32, v interface{}, xv interface{}) (casOut uint64, err error) {

	mutateOps := []gocb.MutateInSpec{
		gocb.UpsertSpec(xattrKey, bytesToRawMessage(xv), UpsertSpecXattr),
		gocb.UpsertSpec(xattrCasPath(xattrKey), gocb.MutationMacroCAS, UpsertSpecXattr),
		gocb.UpsertSpec(xattrCrc32cPath(xattrKey), gocb.MutationMacroValueCRC32c, UpsertSpecXattr),
		gocb.ReplaceSpec("", bytesToRawMessage(v), nil),
	}
	options := &gocb.MutateInOptions{
		Expiry:        CbsExpiryToDuration(exp),
		StoreSemantic: gocb.StoreSemanticsUpsert,
	}
	result, mutateErr := c.MutateIn(k, mutateOps, options)
	if mutateErr != nil {
		return 0, mutateErr
	}
	return uint64(result.Cas()), nil

}

// SubdocUpdateXattr updates the xattr on an existing document. Writes cas and crc32c to the xattr using
// macro expansion.
func (c *Collection) SubdocUpdateXattr(k string, xattrKey string, exp uint32, cas uint64, xv interface{}) (casOut uint64, err error) {
	mutateOps := []gocb.MutateInSpec{
		gocb.UpsertSpec(xattrKey, bytesToRawMessage(xv), UpsertSpecXattr),
		gocb.UpsertSpec(xattrCasPath(xattrKey), gocb.MutationMacroCAS, UpsertSpecXattr),
		gocb.UpsertSpec(xattrCrc32cPath(xattrKey), gocb.MutationMacroValueCRC32c, UpsertSpecXattr),
	}
	options := &gocb.MutateInOptions{
		Expiry:        CbsExpiryToDuration(exp),
		StoreSemantic: gocb.StoreSemanticsUpsert,
		Cas:           gocb.Cas(cas),
	}
	options.Internal.DocFlags = gocb.SubdocDocFlagAccessDeleted

	result, mutateErr := c.MutateIn(k, mutateOps, options)
	if mutateErr != nil {
		return 0, mutateErr
	}
	return uint64(result.Cas()), nil
}

// SubdocUpdateBodyAndXattr updates the document body and xattr of an existing document. Writes cas and crc32c to the xattr using
// macro expansion.
func (c *Collection) SubdocUpdateBodyAndXattr(k string, xattrKey string, exp uint32, cas uint64, v interface{}, xv interface{}) (casOut uint64, err error) {
	mutateOps := []gocb.MutateInSpec{
		gocb.UpsertSpec(xattrKey, bytesToRawMessage(xv), UpsertSpecXattr),
		gocb.UpsertSpec(xattrCasPath(xattrKey), gocb.MutationMacroCAS, UpsertSpecXattr),
		gocb.UpsertSpec(xattrCrc32cPath(xattrKey), gocb.MutationMacroValueCRC32c, UpsertSpecXattr),
		gocb.ReplaceSpec("", bytesToRawMessage(v), nil),
	}
	options := &gocb.MutateInOptions{
		Expiry:        CbsExpiryToDuration(exp),
		StoreSemantic: gocb.StoreSemanticsUpsert,
		Cas:           gocb.Cas(cas),
	}
	result, mutateErr := c.MutateIn(k, mutateOps, options)
	if mutateErr != nil {
		return 0, mutateErr
	}
	return uint64(result.Cas()), nil
}

// SubdocUpdateBodyAndXattr deletes the document body and updates the xattr of an existing document. Writes cas and crc32c to the xattr using
// macro expansion.
func (c *Collection) SubdocUpdateXattrDeleteBody(k, xattrKey string, exp uint32, cas uint64, xv interface{}) (casOut uint64, err error) {
	mutateOps := []gocb.MutateInSpec{
		gocb.UpsertSpec(xattrKey, bytesToRawMessage(xv), UpsertSpecXattr),
		gocb.UpsertSpec(xattrCasPath(xattrKey), gocb.MutationMacroCAS, UpsertSpecXattr),
		gocb.UpsertSpec(xattrCrc32cPath(xattrKey), gocb.MutationMacroValueCRC32c, UpsertSpecXattr),
		gocb.RemoveSpec("", nil),
	}
	options := &gocb.MutateInOptions{
		StoreSemantic: gocb.StoreSemanticsReplace,
		Expiry:        CbsExpiryToDuration(exp),
		Cas:           gocb.Cas(cas),
	}
	result, mutateErr := c.MutateIn(k, mutateOps, options)
	if mutateErr != nil {
		return 0, mutateErr
	}
	return uint64(result.Cas()), nil
}

// SubdocDeleteXattr deletes an xattr of an existing document (or document tombstone)
func (c *Collection) SubdocDeleteXattr(k string, xattrKey string, cas uint64) (err error) {

	mutateOps := []gocb.MutateInSpec{
		gocb.RemoveSpec(xattrKey, RemoveSpecXattr),
	}
	options := &gocb.MutateInOptions{
		Cas: gocb.Cas(cas),
	}
	options.Internal.DocFlags = gocb.SubdocDocFlagAccessDeleted

	_, mutateErr := c.MutateIn(k, mutateOps, options)
	return mutateErr
}

// SubdocDeleteXattr deletes the document body and associated xattr of an existing document.
func (c *Collection) SubdocDeleteBodyAndXattr(k string, xattrKey string) (err error) {
	mutateOps := []gocb.MutateInSpec{
		gocb.RemoveSpec(xattrKey, RemoveSpecXattr),
		gocb.RemoveSpec("", nil),
	}
	options := &gocb.MutateInOptions{
		StoreSemantic: gocb.StoreSemanticsReplace,
	}
	_, mutateErr := c.MutateIn(k, mutateOps, options)
	if mutateErr == nil {
		return nil
	}

	// StatusKeyNotFound returned if document doesn't exist
	if isKVError(mutateErr, memd.StatusKeyNotFound) {
		return ErrNotFound
	}

	// StatusSubDocBadMulti returned if xattr doesn't exist
	if isKVError(mutateErr, memd.StatusSubDocBadMulti) {
		return ErrXattrNotFound
	}
	return mutateErr
}

// SubdocDeleteXattr deletes the document body of an existing document, and updates cas and crc32c in the associated xattr.
func (c *Collection) SubdocDeleteBody(k string, xattrKey string, exp uint32, cas uint64) (casOut uint64, err error) {
	mutateOps := []gocb.MutateInSpec{
		gocb.UpsertSpec(xattrCasPath(xattrKey), gocb.MutationMacroCAS, UpsertSpecXattr),
		gocb.UpsertSpec(xattrCrc32cPath(xattrKey), gocb.MutationMacroValueCRC32c, UpsertSpecXattr),
		gocb.RemoveSpec("", nil),
	}
	options := &gocb.MutateInOptions{
		StoreSemantic: gocb.StoreSemanticsReplace,
		Expiry:        CbsExpiryToDuration(exp),
		Cas:           gocb.Cas(cas),
	}
	result, mutateErr := c.MutateIn(k, mutateOps, options)
	if mutateErr != nil {
		return 0, mutateErr
	}
	return uint64(result.Cas()), nil
}

// isKVError compares the status code of a gocb KeyValueError to the provided code.  If the provided error is
// a gocb.SubDocumentError, checks against that error's InnerError.
func isKVError(err error, code memd.StatusCode) bool {

	switch typedErr := err.(type) {
	case gocb.KeyValueError:
		if typedErr.StatusCode == code {
			return true
		}
	case *gocb.KeyValueError:
		if typedErr.StatusCode == code {
			return true
		}
	case gocbcore.KeyValueError:
		if typedErr.StatusCode == code {
			return true
		}
	case *gocbcore.KeyValueError:
		if typedErr.StatusCode == code {
			return true
		}
	case gocbcore.SubDocumentError:
		return isKVError(typedErr.InnerError, code)
	case *gocbcore.SubDocumentError:
		return isKVError(typedErr.InnerError, code)
	}

	return false
}

// If v is []byte or *[]byte, converts to json.RawMessage to avoid duplicate marshalling by gocb.
func bytesToRawMessage(v interface{}) interface{} {
	switch val := v.(type) {
	case []byte:
		return json.RawMessage(val)
	case *[]byte:
		return json.RawMessage(*val)
	default:
		return v
	}
}
