package keeper

import (
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/lavanet/lava/utils"
	"github.com/lavanet/lava/x/dualstaking/types"
)

func (k msgServer) Delegate(goCtx context.Context, msg *types.MsgDelegate) (*types.MsgDelegateResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)
	return &types.MsgDelegateResponse{}, k.Keeper.DelegateFull(ctx, msg.Creator, msg.Validator, msg.Provider, msg.ChainID, msg.Amount)
}

// DelegateFull uses staking module for to delegate with hooks
func (k Keeper) DelegateFull(ctx sdk.Context, delegator string, validator string, provider string, chainID string, amount sdk.Coin) error {
	_, found := k.specKeeper.GetSpec(ctx, chainID)
	if !found && chainID != types.EMPTY_PROVIDER_CHAINID {
		return utils.LavaFormatWarning("invalid chain ID", fmt.Errorf("chain ID not found"),
			utils.LogAttr("chain_id", chainID))
	}

	valAddr, valErr := sdk.ValAddressFromBech32(validator)
	if valErr != nil {
		return valErr
	}

	validatorType, found := k.stakingKeeper.GetValidator(ctx, valAddr)
	if !found {
		return stakingtypes.ErrNoValidatorFound
	}

	delegatorAddress, err := sdk.AccAddressFromBech32(delegator)
	if err != nil {
		return err
	}

	if _, err = sdk.AccAddressFromBech32(provider); err != nil {
		return err
	}

	if err := utils.ValidateCoins(ctx, k.stakingKeeper.BondDenom(ctx), amount, false); err != nil {
		return err
	}

	_, err = k.stakingKeeper.Delegate(ctx, delegatorAddress, amount.Amount, stakingtypes.Unbonded, validatorType, true)
	if err != nil {
		return err
	}

	err = k.Redelegate(
		ctx,
		delegator,
		types.EMPTY_PROVIDER,
		provider,
		types.EMPTY_PROVIDER_CHAINID,
		chainID,
		amount,
	)

	if err == nil {
		logger := k.Logger(ctx)
		details := map[string]string{
			"delegator": delegator,
			"provider":  provider,
			"chainID":   chainID,
			"amount":    amount.String(),
		}
		utils.LogLavaEvent(ctx, logger, types.DelegateEventName, details, "Delegate")
	}

	return err
}
