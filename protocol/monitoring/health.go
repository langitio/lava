package monitoring

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/gogo/status"
	lvutil "github.com/lavanet/lava/ecosystem/lavavisor/pkg/util"
	"github.com/lavanet/lava/protocol/chainlib"
	"github.com/lavanet/lava/protocol/common"
	"github.com/lavanet/lava/protocol/lavaprotocol"
	"github.com/lavanet/lava/protocol/lavasession"
	"github.com/lavanet/lava/protocol/rpcprovider"
	"github.com/lavanet/lava/utils"
	"github.com/lavanet/lava/utils/rand"
	dualstakingtypes "github.com/lavanet/lava/x/dualstaking/types"
	epochstoragetypes "github.com/lavanet/lava/x/epochstorage/types"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	protocoltypes "github.com/lavanet/lava/x/protocol/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
	subscriptiontypes "github.com/lavanet/lava/x/subscription/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

var QueryRetries = uint64(3)

const (
	BasicQueryRetries = 3
	QuerySleepTime    = 100 * time.Millisecond
	NiceOutputLength  = 40
)

type LavaEntity struct {
	Address      string
	SpecId       string
	ApiInterface string
}

func (e *LavaEntity) String() string {
	if e.SpecId == "" && e.ApiInterface == "" {
		return e.Address
	}
	return fmt.Sprintf("%s | %s | %s", e.Address, e.SpecId, e.ApiInterface)
}

type ReplyData struct {
	block   int64
	latency time.Duration
}

type SubscriptionData struct {
	FullMonthsLeft               uint64
	UsagePercentageLeftThisMonth float64
	DurationLeft                 time.Duration
}

func RunHealth(ctx context.Context,
	clientCtx client.Context,
	subscriptionAddresses []string,
	providerAddresses []string,
	consumerEndpoints []*lavasession.RPCEndpoint,
	referenceEndpoints []*lavasession.RPCEndpoint,
	prometheusListenAddr string,
) (*HealthResults, error) {
	specQuerier := spectypes.NewQueryClient(clientCtx)
	healthResults := &HealthResults{
		LatestBlocks:       map[string]int64{},
		ProviderData:       map[LavaEntity]ReplyData{},
		ConsumerBlocks:     map[LavaEntity]int64{},
		SubscriptionsData:  map[string]SubscriptionData{},
		FrozenProviders:    map[LavaEntity]struct{}{},
		UnhealthyProviders: map[LavaEntity]string{},
		UnhealthyConsumers: map[LavaEntity]string{},
		Specs:              map[string]*spectypes.Spec{},
	}
	currentBlock := int64(0)
	for i := 0; i < BasicQueryRetries; i++ {
		resultStatus, err := clientCtx.Client.Status(ctx)
		if err == nil {
			currentBlock = resultStatus.SyncInfo.LatestBlockHeight
			break
		}
		time.Sleep(QuerySleepTime)
	}
	if currentBlock == 0 {
		return nil, utils.LavaFormatError("failed querying lava chain for block", nil)
	}
	var err error
	var allChains *spectypes.QueryShowAllChainsResponse
	for i := 0; i < BasicQueryRetries; i++ {
		allChains, err = specQuerier.ShowAllChains(ctx, &spectypes.QueryShowAllChainsRequest{})
		if err == nil {
			break
		}
		time.Sleep(QuerySleepTime)
	}
	if err != nil {
		return nil, err
	}
	chainIdToApiInterfaces := map[string][]string{}
	for _, chainInfo := range allChains.ChainInfoList {
		if len(chainInfo.EnabledApiInterfaces) > 0 {
			chainIdToApiInterfaces[chainInfo.ChainID] = chainInfo.EnabledApiInterfaces
		}
	}
	errCh := make(chan error, 1)

	// get a list of all necessary specs for the test
	dualStakingQuerier := dualstakingtypes.NewQueryClient(clientCtx)
	var wgspecs sync.WaitGroup
	wgspecs.Add(len(providerAddresses))

	processProvider := func(providerAddress string) {
		defer wgspecs.Done()
		var err error
		for i := 0; i < BasicQueryRetries; i++ {
			var response *dualstakingtypes.QueryDelegatorProvidersResponse
			queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			response, err = dualStakingQuerier.DelegatorProviders(queryCtx, &dualstakingtypes.QueryDelegatorProvidersRequest{
				Delegator:   providerAddress,
				WithPending: false,
			})
			cancel()
			if err != nil || response == nil {
				time.Sleep(QuerySleepTime)
				continue
			}
			delegations := response.GetDelegations()
			for _, delegation := range delegations {
				if delegation.Provider == providerAddress {
					healthResults.setSpec(&spectypes.Spec{Index: delegation.ChainID})
					for _, apiInterface := range chainIdToApiInterfaces[delegation.ChainID] {
						healthResults.SetProviderData(LavaEntity{
							Address:      providerAddress,
							SpecId:       delegation.ChainID,
							ApiInterface: apiInterface,
						}, ReplyData{})
					}
				}
			}
			return
		}
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}

	for _, providerAddress := range providerAddresses {
		go processProvider(providerAddress)
	}

	for _, consumerEndpoint := range consumerEndpoints {
		healthResults.setSpec(&spectypes.Spec{Index: consumerEndpoint.ChainID})
	}

	for _, referenceEndpoint := range referenceEndpoints {
		healthResults.setSpec(&spectypes.Spec{Index: referenceEndpoint.ChainID})
	}

	wgspecs.Wait()
	if len(errCh) > 0 {
		return nil, utils.LavaFormatWarning("[-] process providers specs", <-errCh)
	}
	// add specs
	specs := healthResults.getSpecs()
	processSpec := func(specId string) {
		defer wgspecs.Done()
		var err error
		for i := 0; i < BasicQueryRetries; i++ {
			var specResp *spectypes.QueryGetSpecResponse
			queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			specResp, err = specQuerier.Spec(queryCtx, &spectypes.QueryGetSpecRequest{
				ChainID: specId,
			})
			cancel()
			if err != nil || specResp == nil {
				time.Sleep(QuerySleepTime)
				continue
			}
			spec := specResp.GetSpec()
			healthResults.setSpec(&spec)
			return
		}
		select {
		case errCh <- err:
		default:
		}
	}
	wgspecs.Add(len(specs))
	// populate the specs
	utils.LavaFormatDebug("[+] populating specs")
	for specId := range specs {
		go processSpec(specId)
	}

	wgspecs.Wait()
	if len(errCh) > 0 {
		return nil, utils.LavaFormatWarning("[-] populating specs", <-errCh)
	}
	pairingQuerier := pairingtypes.NewQueryClient(clientCtx)
	utils.LavaFormatDebug("[+] getting provider entries")
	stakeEntries := map[LavaEntity]epochstoragetypes.StakeEntry{}
	var mutex sync.Mutex // Mutex to protect concurrent access to stakeEntries
	wgspecs.Add(len(healthResults.getSpecs()))
	processSpecProviders := func(specId string) {
		defer wgspecs.Done()
		var err error
		for i := 0; i < BasicQueryRetries; i++ {
			var response *pairingtypes.QueryProvidersResponse
			queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			response, err = pairingQuerier.Providers(queryCtx, &pairingtypes.QueryProvidersRequest{
				ChainID:    specId,
				ShowFrozen: true,
			})
			cancel()
			if err != nil || response == nil {
				time.Sleep(QuerySleepTime)
				continue
			}

			for _, providerEntry := range response.StakeEntry {
				providerKey := LavaEntity{
					Address: providerEntry.Address,
					SpecId:  specId,
				}
				apiInterfaces := chainIdToApiInterfaces[specId]
				// just to check if this is a provider we need to check we need one of the apiInterfaces
				if len(apiInterfaces) == 0 {
					utils.LavaFormatError("invalid state len(apiInterfaces) == 0", nil, utils.LogAttr("specId", specId))
					// shouldn't happen
					continue
				}
				lookupKey := LavaEntity{
					Address:      providerEntry.Address,
					SpecId:       specId,
					ApiInterface: apiInterfaces[0],
				}

				mutex.Lock() // Lock before updating stakeEntries
				if _, ok := healthResults.getProviderData(lookupKey); ok {
					if providerEntry.StakeAppliedBlock > uint64(currentBlock) {
						healthResults.FreezeProvider(providerKey)
					} else {
						stakeEntries[providerKey] = providerEntry
					}
				}
				mutex.Unlock()
			}
			return
		}
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}
	// get provider stake entries
	for specId := range healthResults.getSpecs() {
		go processSpecProviders(specId)
	}
	wgspecs.Wait()
	if len(errCh) > 0 {
		return nil, utils.LavaFormatWarning("[-] processing providers entries", <-errCh)
	}
	utils.LavaFormatDebug("[+] checking subscriptions")
	err = checkSubscriptions(ctx, clientCtx, subscriptionAddresses, healthResults)
	if err != nil {
		return nil, utils.LavaFormatWarning("[-] checking subscriptions", <-errCh)
	}
	utils.LavaFormatDebug("[+] checking providers")
	err = CheckProviders(ctx, clientCtx, healthResults, stakeEntries)
	if err != nil {
		return nil, utils.LavaFormatWarning("[-] checking providers health", <-errCh)
	}
	utils.LavaFormatDebug("[+] checking consumers")
	err = CheckConsumersAndReferences(ctx, clientCtx, referenceEndpoints, consumerEndpoints, healthResults)
	if err != nil {
		return nil, utils.LavaFormatWarning("[-] checking consumers and references", <-errCh)
	}
	utils.LavaFormatDebug("health results", utils.LogAttr("dump", healthResults))
	return healthResults, nil
}

func CheckConsumersAndReferences(ctx context.Context,
	clientCtx client.Context,
	referenceEndpoints []*lavasession.RPCEndpoint,
	consumerEndpoints []*lavasession.RPCEndpoint,
	healthResults *HealthResults,
) error {
	// populate data from providers
	for entry, data := range healthResults.ProviderData {
		providerBlock := data.block
		specId := entry.SpecId
		healthResults.updateLatestBlock(specId, providerBlock)
	}
	errCh := make(chan error, 1)
	queryEndpoint := func(endpoint *lavasession.RPCEndpoint, isReference bool) error {
		chainParser, err := chainlib.NewChainParser(endpoint.ApiInterface)
		if err != nil {
			return err
		}
		spec := healthResults.getSpec(endpoint.ChainID)
		if spec == nil {
			return err
		}
		chainParser.SetSpec(*spec)
		compatibleEndpoint := &lavasession.RPCProviderEndpoint{
			NetworkAddress: lavasession.NetworkAddressData{},
			ChainID:        endpoint.ChainID,
			ApiInterface:   endpoint.ApiInterface,
			Geolocation:    0,
			NodeUrls: []common.NodeUrl{
				{
					Url: endpoint.NetworkAddress,
				},
			},
		}
		var chainProxy chainlib.ChainRouter
		for i := uint64(0); i <= QueryRetries; i++ {
			sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			chainProxy, err = chainlib.GetChainRouter(sendCtx, 1, compatibleEndpoint, chainParser)
			cancel()
			if err == nil {
				break
			}
		}
		if err != nil {
			utils.LavaFormatDebug("failed creating chain proxy, continuing with others endpoints", utils.LogAttr("reference", isReference), utils.Attribute{Key: "endpoint", Value: compatibleEndpoint})
			if !isReference {
				healthResults.updateConsumerError(endpoint, err)
			}
			return nil
		}
		chainFetcher := chainlib.NewChainFetcher(ctx, &chainlib.ChainFetcherOptions{ChainRouter: chainProxy, ChainParser: chainParser, Endpoint: compatibleEndpoint, Cache: nil})
		var latestBlock int64
		for i := uint64(0); i <= QueryRetries; i++ {
			sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			latestBlock, err = chainFetcher.FetchLatestBlockNum(sendCtx)
			cancel()
			if err == nil {
				break
			}
		}
		if err != nil {
			if isReference {
				utils.LavaFormatDebug("failed querying latest block from reference", utils.LogAttr("endpoint", endpoint.String()))
			} else {
				healthResults.updateConsumerError(endpoint, err)
			}
			return nil
		}
		if !isReference {
			healthResults.updateConsumer(endpoint, latestBlock)
		}
		healthResults.updateLatestBlock(endpoint.ChainID, latestBlock)
		return nil
	}

	// populate data from references
	var wg sync.WaitGroup
	wg.Add(len(referenceEndpoints))
	wg.Add(len(consumerEndpoints))
	for _, endpoint := range referenceEndpoints {
		go func(ep *lavasession.RPCEndpoint) {
			// Decrement the WaitGroup counter when the goroutine completes
			defer wg.Done()
			err := queryEndpoint(ep, true)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(endpoint)
	}
	// query our consumers
	for _, endpoint := range consumerEndpoints {
		go func(ep *lavasession.RPCEndpoint) {
			// Decrement the WaitGroup counter when the goroutine completes
			defer wg.Done()
			err := queryEndpoint(ep, false)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(endpoint)
	}
	wg.Wait()
	if len(errCh) > 0 {
		return <-errCh
	}
	return nil
}

func checkSubscriptions(ctx context.Context, clientCtx client.Context, subscriptionAddresses []string, healthResults *HealthResults) error {
	subscriptionQuerier := subscriptiontypes.NewQueryClient(clientCtx)
	var wg sync.WaitGroup
	wg.Add(len(subscriptionAddresses))
	errCh := make(chan error, 1)
	for _, subscriptionAddr := range subscriptionAddresses {
		go func(addr string) {
			defer wg.Done()
			var err error
			for i := 0; i < BasicQueryRetries; i++ {
				var response *subscriptiontypes.QueryCurrentResponse
				queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				response, err = subscriptionQuerier.Current(queryCtx, &subscriptiontypes.QueryCurrentRequest{
					Consumer: addr,
				})
				cancel()
				if err != nil {
					time.Sleep(QuerySleepTime)
					continue
				}
				fullMonthsLeft := uint64(0)
				if response.Sub.DurationLeft > 0 {
					// DurationLeft is 0 when expired only, it is 1 for the last month
					fullMonthsLeft = response.Sub.DurationLeft - 1
				}
				healthResults.setSubscriptionData(addr, SubscriptionData{
					FullMonthsLeft:               fullMonthsLeft,
					UsagePercentageLeftThisMonth: float64(response.Sub.MonthCuLeft) / float64(response.Sub.MonthCuTotal),
					DurationLeft:                 time.Until(time.Unix(int64(response.Sub.MonthExpiryTime), 0)),
				})
				break
			}
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(subscriptionAddr)
	}
	wg.Wait()
	if len(errCh) > 0 {
		return <-errCh
	}
	return nil
}

func CheckProviders(ctx context.Context, clientCtx client.Context, healthResults *HealthResults, providerEntries map[LavaEntity]epochstoragetypes.StakeEntry) error {
	protocolQuerier := protocoltypes.NewQueryClient(clientCtx)
	var param *protocoltypes.QueryParamsResponse
	var err error
	for i := 0; i < BasicQueryRetries; i++ {
		param, err = protocolQuerier.Params(ctx, &protocoltypes.QueryParamsRequest{})
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	lavaVersion := param.GetParams().Version
	if err != nil {
		return err
	}
	targetVersion := lvutil.ParseToSemanticVersion(lavaVersion.ProviderTarget)
	var wg sync.WaitGroup
	wg.Add(len(providerEntries))

	checkProviderEndpoints := func(providerEntry epochstoragetypes.StakeEntry) {
		defer wg.Done()
		for _, endpoint := range providerEntry.Endpoints {
			checkOneProvider := func(endpoint epochstoragetypes.Endpoint, apiInterface string, addon string, providerEntry epochstoragetypes.StakeEntry) (time.Duration, string, int64, error) {
				cswp := lavasession.ConsumerSessionsWithProvider{}
				relayerClientPt, conn, err := cswp.ConnectRawClientWithTimeout(ctx, endpoint.IPPORT)
				if err != nil {
					utils.LavaFormatDebug("failed connecting to provider endpoint", utils.LogAttr("error", err), utils.Attribute{Key: "apiInterface", Value: apiInterface}, utils.Attribute{Key: "addon", Value: addon}, utils.Attribute{Key: "chainID", Value: providerEntry.Chain}, utils.Attribute{Key: "network address", Value: endpoint.IPPORT})
					return 0, "", 0, err
				}
				defer conn.Close()
				relayerClient := *relayerClientPt
				guid := uint64(rand.Int63())
				relaySentTime := time.Now()
				probeReq := &pairingtypes.ProbeRequest{
					Guid:         guid,
					SpecId:       providerEntry.Chain,
					ApiInterface: apiInterface,
				}
				var trailer metadata.MD
				probeResp, err := relayerClient.Probe(ctx, probeReq, grpc.Trailer(&trailer))
				if err != nil {
					utils.LavaFormatDebug("failed probing provider endpoint", utils.LogAttr("error", err), utils.Attribute{Key: "apiInterface", Value: apiInterface}, utils.Attribute{Key: "addon", Value: addon}, utils.Attribute{Key: "chainID", Value: providerEntry.Chain}, utils.Attribute{Key: "network address", Value: endpoint.IPPORT})
					return 0, "", 0, err
				}
				versions := strings.Join(trailer.Get(common.VersionMetadataKey), ",")
				relayLatency := time.Since(relaySentTime)
				if guid != probeResp.GetGuid() {
					return 0, versions, 0, utils.LavaFormatWarning("probe returned invalid value", err, utils.Attribute{Key: "returnedGuid", Value: probeResp.GetGuid()}, utils.Attribute{Key: "guid", Value: guid}, utils.Attribute{Key: "apiInterface", Value: apiInterface}, utils.Attribute{Key: "addon", Value: addon}, utils.Attribute{Key: "chainID", Value: providerEntry.Chain}, utils.Attribute{Key: "network address", Value: endpoint.IPPORT})
				}

				// CORS check
				if err := rpcprovider.PerformCORSCheck(endpoint); err != nil {
					return 0, versions, 0, err
				}

				relayRequest := &pairingtypes.RelayRequest{
					RelaySession: &pairingtypes.RelaySession{SpecId: providerEntry.Chain},
					RelayData:    &pairingtypes.RelayPrivateData{ApiInterface: apiInterface, Addon: addon},
				}
				_, err = relayerClient.Relay(ctx, relayRequest)
				if err == nil {
					return 0, "", 0, utils.LavaFormatWarning("relay Without signature did not error, unexpected", nil, utils.Attribute{Key: "apiInterface", Value: apiInterface}, utils.Attribute{Key: "addon", Value: addon}, utils.Attribute{Key: "chainID", Value: providerEntry.Chain}, utils.Attribute{Key: "network address", Value: endpoint.IPPORT})
				}
				code := status.Code(err)
				if code != codes.Code(lavasession.EpochMismatchError.ABCICode()) {
					return 0, versions, 0, utils.LavaFormatWarning("relay returned unexpected error", err, utils.Attribute{Key: "apiInterface", Value: apiInterface}, utils.Attribute{Key: "addon", Value: addon}, utils.Attribute{Key: "chainID", Value: providerEntry.Chain}, utils.Attribute{Key: "network address", Value: endpoint.IPPORT})
				}
				return relayLatency, versions, probeResp.GetLatestBlock(), nil
			}
			endpointServices := endpoint.GetSupportedServices()
			if len(endpointServices) == 0 {
				utils.LavaFormatWarning("endpoint has no supported services", nil, utils.Attribute{Key: "endpoint", Value: endpoint})
			}
			for _, endpointService := range endpointServices {
				providerKey := LavaEntity{
					Address:      providerEntry.Address,
					SpecId:       providerEntry.Chain,
					ApiInterface: endpointService.ApiInterface,
				}
				probeLatency, version, latestBlockFromProbe, err := checkOneProvider(endpoint, endpointService.ApiInterface, endpointService.Addon, providerEntry)
				if err != nil {
					errMsg := prettifyProviderError(err)
					healthResults.SetUnhealthyProvider(providerKey, errMsg)
					continue
				}
				parsedVer := lvutil.ParseToSemanticVersion(strings.TrimPrefix(version, "v"))
				if lvutil.IsVersionLessThan(parsedVer, targetVersion) || lvutil.IsVersionGreaterThan(parsedVer, targetVersion) {
					healthResults.SetUnhealthyProvider(providerKey, "Version:"+version+" should be: "+lavaVersion.ProviderTarget)
					continue
				}
				latestData := ReplyData{
					block:   latestBlockFromProbe,
					latency: probeLatency,
				}
				healthResults.SetProviderData(providerKey, latestData)
			}
		}
	}

	for _, providerEntry := range providerEntries {
		go checkProviderEndpoints(providerEntry)
	}
	wg.Wait()
	return nil
}

func prettifyProviderError(err error) string {
	code := status.Code(err)
	if code == codes.Code(lavaprotocol.UnhandledRelayReceiverError.ABCICode()) {
		return "provider running with unhandled support"
	}
	if code == codes.Code(lavaprotocol.DisabledRelayReceiverError.ABCICode()) {
		return "provider running with disabled support due to verification"
	}
	if len(err.Error()) < NiceOutputLength {
		return err.Error()
	}
	return err.Error()[:NiceOutputLength]
}
