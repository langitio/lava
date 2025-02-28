package rpcInterfaceMessages

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/lavanet/lava/protocol/chainlib/chainproxy"
	"github.com/lavanet/lava/protocol/parser"
)

type RestMessage struct {
	Msg      []byte
	Path     string
	SpecPath string
	chainproxy.BaseMessage
}

// GetParams will be deprecated after we remove old client
// Currently needed because of parser.RPCInput interface
func (cp RestMessage) GetParams() interface{} {
	urlObj, err := url.Parse(cp.Path)
	if err != nil {
		return nil
	}
	parsedMethod := urlObj.Path
	objectSpec := strings.Split(cp.SpecPath, "/")
	objectPath := strings.Split(parsedMethod, "/")

	parameters := map[string]interface{}{}

	for index, element := range objectSpec {
		if strings.Contains(element, "{") {
			element = strings.Trim(element, "{}")
			parameters[element] = objectPath[index]
		}
	}
	for key, values := range urlObj.Query() {
		parameters[key] = strings.Join(values, ",")
	}
	if len(parameters) == 0 {
		return nil
	}
	return parameters
}

func (rm *RestMessage) UpdateLatestBlockInMessage(latestBlock uint64, modifyContent bool) (success bool) {
	// return rm.SetLatestBlockWithHeader(latestBlock, modifyContent)
	// removed until behaviour inconsistency with the cosmos sdk header is solved
	return false
	// if !done else we need a different setter
}

// GetResult will be deprecated after we remove old client
// Currently needed because of parser.RPCInput interface
func (cp RestMessage) GetResult() json.RawMessage {
	return nil
}

// ParseBlock parses default block number from string to int
func (cp RestMessage) ParseBlock(inp string) (int64, error) {
	return parser.ParseDefaultBlockParameter(inp)
}
