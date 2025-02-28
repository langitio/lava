package keeper_test

import (
	"strconv"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	keepertest "github.com/lavanet/lava/testutil/keeper"
	"github.com/lavanet/lava/x/conflict/types"
)

// Prevent strconv unused error
var _ = strconv.IntSize

func TestProviderConflicts(t *testing.T) {
	keeper, ctx := keepertest.ConflictKeeper(t)
	wctx := sdk.WrapSDKContext(ctx)
	msgs := createNConflictVote(keeper, ctx, 7)

	const (
		FIRST_PROVIDER                  = 0
		SECOND_PROVIDER                 = 1
		NONE_OF_THE_PROVIDERS           = 2
		NOT_VOTED                       = 3
		VOTED                           = 4
		PROVIDER_REPORTED_AND_NOT_VOTED = 5
		PROVIDER_REPORTED_AND_VOTED     = 6
	)

	var providers []string
	for i := range msgs {
		providers = append(providers, "p"+strconv.Itoa(i))
	}

	for i, msg := range msgs {
		switch i {
		case FIRST_PROVIDER:
			msg.FirstProvider.Account = providers[FIRST_PROVIDER]
		case SECOND_PROVIDER:
			msg.SecondProvider.Account = providers[SECOND_PROVIDER]
		case NONE_OF_THE_PROVIDERS:
			msg.FirstProvider.Account = providers[NONE_OF_THE_PROVIDERS]
			msg.SecondProvider.Account = providers[NONE_OF_THE_PROVIDERS]
		case NOT_VOTED:
			msg.Votes = append(msg.Votes, types.Vote{Address: providers[NOT_VOTED], Result: types.NoVote})
		case VOTED:
			msg.Votes = append(msg.Votes, types.Vote{Address: providers[VOTED], Result: types.Provider0})
		case PROVIDER_REPORTED_AND_NOT_VOTED:
			msg.FirstProvider.Account = providers[PROVIDER_REPORTED_AND_NOT_VOTED]
			msg.Votes = append(msg.Votes, types.Vote{Address: providers[PROVIDER_REPORTED_AND_NOT_VOTED], Result: types.NoVote})
		case PROVIDER_REPORTED_AND_VOTED:
			msg.FirstProvider.Account = providers[PROVIDER_REPORTED_AND_VOTED]
			msg.Votes = append(msg.Votes, types.Vote{Address: providers[PROVIDER_REPORTED_AND_VOTED], Result: types.Provider0})
		}

		keeper.SetConflictVote(ctx, msg)
	}

	for _, tc := range []struct {
		desc             string
		provider         string
		expectedReported []string
		expectedNotVoted []string
	}{
		{
			desc:             "First provider",
			provider:         providers[FIRST_PROVIDER],
			expectedReported: []string{strconv.Itoa(FIRST_PROVIDER)},
			expectedNotVoted: []string{},
		},
		{
			desc:             "Second provider",
			provider:         providers[SECOND_PROVIDER],
			expectedReported: []string{strconv.Itoa(SECOND_PROVIDER)},
			expectedNotVoted: []string{},
		},
		{
			desc:             "None of the providers",
			provider:         "dummy",
			expectedReported: []string{},
			expectedNotVoted: []string{},
		},
		{
			desc:             "Not voted",
			provider:         providers[NOT_VOTED],
			expectedReported: []string{},
			expectedNotVoted: []string{strconv.Itoa(NOT_VOTED)},
		},
		{
			desc:             "Voted",
			provider:         providers[VOTED],
			expectedReported: []string{},
			expectedNotVoted: []string{},
		},
		{
			desc:             "Provider reported and not voted",
			provider:         providers[PROVIDER_REPORTED_AND_NOT_VOTED],
			expectedReported: []string{strconv.Itoa(PROVIDER_REPORTED_AND_NOT_VOTED)},
			expectedNotVoted: []string{strconv.Itoa(PROVIDER_REPORTED_AND_NOT_VOTED)},
		},
		{
			desc:             "First Provider and voted",
			provider:         providers[PROVIDER_REPORTED_AND_VOTED],
			expectedReported: []string{strconv.Itoa(PROVIDER_REPORTED_AND_VOTED)},
			expectedNotVoted: []string{},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			res, err := keeper.ProviderConflicts(wctx, &types.QueryProviderConflictsRequest{Provider: tc.provider})
			require.NoError(t, err)
			require.ElementsMatch(t, res.NotVoted, tc.expectedNotVoted)
			require.ElementsMatch(t, res.Reported, tc.expectedReported)
		})
	}
}
