package chainlib

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/favicon"
	common "github.com/lavanet/lava/protocol/common"
	"github.com/lavanet/lava/protocol/metrics"
	"github.com/lavanet/lava/utils"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
	"google.golang.org/grpc/metadata"
)

const (
	ContextUserValueKeyDappID = "dappID"
	RetryListeningInterval    = 10 // seconds
	debug                     = false
)

type VerificationKey struct {
	Extension string
	Addon     string
}

type VerificationContainer struct {
	ConnectionType string
	Name           string
	ParseDirective spectypes.ParseDirective
	Value          string
	LatestDistance uint64
	Severity       spectypes.Verification_VerificationSeverity
	VerificationKey
}

type TaggedContainer struct {
	Parsing       *spectypes.ParseDirective
	ApiCollection *spectypes.ApiCollection
}

type ApiContainer struct {
	api           *spectypes.Api
	collectionKey CollectionKey
}

type ApiKey struct {
	Name           string
	ConnectionType string
}

type CollectionKey struct {
	ConnectionType string
	InternalPath   string
	Addon          string
}

type BaseChainProxy struct {
	ErrorHandler
	averageBlockTime time.Duration
	NodeUrl          common.NodeUrl
	ChainID          string
}

// returns the node url and chain id for that proxy.
func (bcp *BaseChainProxy) GetChainProxyInformation() (common.NodeUrl, string) {
	return bcp.NodeUrl, bcp.ChainID
}

func extractDappIDFromFiberContext(c *fiber.Ctx) (dappID string) {
	// Read the dappID from the headers
	dappID = c.Get("dapp-id")
	if dappID == "" {
		dappID = generateNewDappID()
	}
	return dappID
}

// extractDappIDFromGrpcHeader extracts dappID from GRPC header
func extractDappIDFromGrpcHeader(metadataValues metadata.MD) string {
	dappId := generateNewDappID()
	if values, ok := metadataValues["dapp-id"]; ok && len(values) > 0 {
		dappId = values[0]
	}
	return dappId
}

// generateNewDappID generates default dappID
// In future we can also implement unique dappID generation
func generateNewDappID() string {
	return "DefaultDappID"
}

func constructFiberCallbackWithHeaderAndParameterExtraction(callbackToBeCalled fiber.Handler, isMetricEnabled bool) fiber.Handler {
	webSocketCallback := callbackToBeCalled
	handler := func(c *fiber.Ctx) error {
		// Extract dappID from headers
		dappID := extractDappIDFromFiberContext(c)

		// Store dappID in the local context
		c.Locals("dapp-id", dappID)

		if isMetricEnabled {
			c.Locals(metrics.RefererHeaderKey, c.Get(metrics.RefererHeaderKey, ""))
			c.Locals(metrics.UserAgentHeaderKey, c.Get(metrics.UserAgentHeaderKey, ""))
			c.Locals(metrics.OriginHeaderKey, c.Get(metrics.OriginHeaderKey, ""))
		}
		return webSocketCallback(c) // uses external dappID
	}
	return handler
}

func convertToJsonError(errorMsg string) string {
	jsonResponse, err := json.Marshal(fiber.Map{
		"error": errorMsg,
	})
	if err != nil {
		return `{"error": "Failed to marshal error response to json"}`
	}

	return string(jsonResponse)
}

func addAttributeToError(key, value, errorMessage string) string {
	return errorMessage + fmt.Sprintf(`, "%v": "%v"`, key, value)
}

// rpc default endpoint should be websocket. otherwise return an error
func verifyRPCEndpoint(endpoint string) {
	u, err := url.Parse(endpoint)
	if err != nil {
		utils.LavaFormatFatal("unparsable url", err, utils.Attribute{Key: "url", Value: endpoint})
	}
	switch u.Scheme {
	case "ws", "wss":
		return
	default:
		utils.LavaFormatWarning("URL scheme should be websocket (ws/wss), got: "+u.Scheme, nil)
	}
}

// rpc default endpoint should be websocket. otherwise return an error
func verifyTendermintEndpoint(endpoints []common.NodeUrl) (websocketEndpoint, httpEndpoint common.NodeUrl) {
	for _, endpoint := range endpoints {
		u, err := url.Parse(endpoint.Url)
		if err != nil {
			utils.LavaFormatFatal("unparsable url", err, utils.Attribute{Key: "url", Value: endpoint.UrlStr()})
		}
		switch u.Scheme {
		case "http", "https":
			httpEndpoint = endpoint
		case "ws", "wss":
			websocketEndpoint = endpoint
		default:
			utils.LavaFormatFatal("URL scheme should be websocket (ws/wss) or (http/https), got: "+u.Scheme, nil)
		}
	}

	if websocketEndpoint.String() == "" || httpEndpoint.String() == "" {
		utils.LavaFormatError("Tendermint Provider was not provided with both http and websocket urls. please provide both", nil,
			utils.Attribute{Key: "websocket", Value: websocketEndpoint.String()}, utils.Attribute{Key: "http", Value: httpEndpoint.String()})
		if httpEndpoint.String() != "" {
			return httpEndpoint, httpEndpoint
		} else {
			utils.LavaFormatFatal("Tendermint Provider was not provided with http url. please provide a url that starts with http/https", nil)
		}
	}
	return websocketEndpoint, httpEndpoint
}

func ListenWithRetry(app *fiber.App, address string) {
	for {
		err := app.Listen(address)
		if err != nil {
			utils.LavaFormatError("app.Listen(listenAddr)", err)
		}
		time.Sleep(RetryListeningInterval * time.Second)
	}
}

func GetListenerWithRetryGrpc(protocol, addr string) net.Listener {
	for {
		lis, err := net.Listen(protocol, addr)
		if err == nil {
			return lis
		}
		utils.LavaFormatError("failure setting up listener, net.Listen(protocol, addr)", err, utils.Attribute{Key: "listenAddr", Value: addr})
		time.Sleep(RetryListeningInterval * time.Second)
		utils.LavaFormatWarning("Attempting connection retry", nil)
	}
}

// rest request headers are formatted like map[string]string
func convertToMetadataMap(md map[string][]string) []pairingtypes.Metadata {
	metadata := make([]pairingtypes.Metadata, len(md))
	indexer := 0
	for k, v := range md {
		metadata[indexer] = pairingtypes.Metadata{Name: k, Value: strings.Join(v, ", ")}
		indexer += 1
	}
	return metadata
}

// rest response headers / grpc headers are formatted like map[string][]string
func convertToMetadataMapOfSlices(md map[string][]string) []pairingtypes.Metadata {
	metadata := make([]pairingtypes.Metadata, len(md))
	indexer := 0
	for k, v := range md {
		metadata[indexer] = pairingtypes.Metadata{Name: k, Value: v[0]}
		indexer += 1
	}
	return metadata
}

func convertRelayMetaDataToMDMetaData(md []pairingtypes.Metadata) metadata.MD {
	responseMetaData := make(metadata.MD)
	for _, v := range md {
		responseMetaData[v.Name] = append(responseMetaData[v.Name], v.Value)
	}
	return responseMetaData
}

// split two requested blocks to the most advanced and most behind
// the hierarchy is as follows:
// NOT_APPLICABLE
// LATEST_BLOCK
// PENDING_BLOCK
// SAFE
// FINALIZED
// numeric value (descending)
// EARLIEST
func CompareRequestedBlockInBatch(firstRequestedBlock int64, second int64) (latestCombinedBlock int64, earliestCombinedBlock int64) {
	if firstRequestedBlock == spectypes.EARLIEST_BLOCK {
		return second, firstRequestedBlock
	}
	if second == spectypes.EARLIEST_BLOCK {
		return firstRequestedBlock, second
	}

	returnBigger := func(in_first int64, in_second int64) (int64, int64) {
		if in_first > in_second {
			return in_first, in_second
		}
		return in_second, in_first
	}

	if firstRequestedBlock < 0 {
		if second < 0 {
			// both are negative
			return returnBigger(firstRequestedBlock, second)
		}
		// first is negative non earliest second is positive
		return firstRequestedBlock, second
	}
	if second < 0 {
		// second is negative non earliest first is positive
		return second, firstRequestedBlock
	}
	// both are positive
	return returnBigger(firstRequestedBlock, second)
}

func GetRelayTimeout(chainMessage ChainMessage, chainParser ChainParser, timeouts int) time.Duration {
	if chainMessage.TimeoutOverride() != 0 {
		return chainMessage.TimeoutOverride()
	}
	// Calculate extra RelayTimeout
	extraRelayTimeout := time.Duration(0)
	if IsHangingApi(chainMessage) {
		_, extraRelayTimeout, _, _ = chainParser.ChainBlockStats()
	}
	relayTimeAddition := common.GetTimePerCu(GetComputeUnits(chainMessage))
	if chainMessage.GetApi().TimeoutMs > 0 {
		relayTimeAddition = time.Millisecond * time.Duration(chainMessage.GetApi().TimeoutMs)
	}
	// Set relay timout, increase it every time we fail a relay on timeout
	return extraRelayTimeout + time.Duration(timeouts+1)*relayTimeAddition + common.AverageWorldLatency
}

// setup a common preflight and cors configuration allowing wild cards and preflight caching.
func createAndSetupBaseAppListener(cmdFlags common.ConsumerCmdFlags, healthCheckPath string, healthReporter HealthReporter) *fiber.App {
	app := fiber.New(fiber.Config{})
	app.Use(favicon.New())
	app.Use(func(c *fiber.Ctx) error {
		// we set up wild card by default.
		c.Set("Access-Control-Allow-Origin", cmdFlags.OriginFlag)
		// Handle preflight requests directly
		if c.Method() == "OPTIONS" {
			// set up all allowed methods.
			c.Set("Access-Control-Allow-Methods", cmdFlags.MethodsFlag)
			// allow headers
			c.Set("Access-Control-Allow-Headers", cmdFlags.HeadersFlag)
			// allow credentials
			c.Set("Access-Control-Allow-Credentials", cmdFlags.CredentialsFlag)
			// Cache preflight request for 24 hours (in seconds)
			c.Set("Access-Control-Max-Age", cmdFlags.CDNCacheDuration)
			return c.SendStatus(fiber.StatusNoContent)
		}
		if c.Method() == "DELETE" {
			return c.SendStatus(fiber.StatusNoContent)
		}
		return c.Next()
	})

	app.Get(healthCheckPath, func(fiberCtx *fiber.Ctx) error {
		if healthReporter.IsHealthy() {
			fiberCtx.Status(http.StatusOK)
			return fiberCtx.SendString("Health status OK")
		} else {
			fiberCtx.Status(http.StatusServiceUnavailable)
			return fiberCtx.SendString("Health status Failure")
		}
	})

	return app
}
