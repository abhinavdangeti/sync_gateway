package db

import (
	"errors"
	"net/http"

	"github.com/couchbase/sync_gateway/base"
)

// QueryParams retrieves the channels associated with the byChannels a replication filter
// from the generic queryParams interface{}.
// The Channels may be passed as a JSON array of strings directly,
// or embedded in a JSON object with the "channels" property and array value
func ChannelsFromQueryParams(queryParams interface{}) (channels []string, err error) {

	var chanarray []interface{}
	if paramsmap, ok := queryParams.(map[string]interface{}); ok {
		if chanarray, ok = paramsmap["channels"].([]interface{}); !ok {
			return nil, errors.New("/_replicate sync_gateway/bychannel filter; query_params missing channels property")
		}
	} else if chanarray, ok = queryParams.([]interface{}); ok {
		// query params is an array and chanarray has been set, now drop out of if-then-else for processing
	} else {
		return nil, base.HTTPErrorf(http.StatusBadRequest, "/_replicate sync_gateway/bychannel filter; Bad channels array")
	}
	if len(chanarray) > 0 {
		channels = make([]string, len(chanarray))
		for i := range chanarray {
			if channel, ok := chanarray[i].(string); ok {
				channels[i] = channel
			} else {
				return nil, errors.New("/_replicate sync_gateway/bychannel filter; Bad channel name")
			}
		}
	}
	return channels, nil
}