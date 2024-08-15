package interchaintest

import (
	"context"
	"fmt"
	"testing"

	"cosmossdk.io/math"
	"github.com/strangelove-ventures/interchaintest/v7/dockerutil"
	"github.com/strangelove-ventures/interchaintest/v7/ibc"
	"github.com/strangelove-ventures/interchaintest/v7/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// GetAndFundTestUserWithMnemonic restores a user using the given mnemonic
// and funds it with the native chain denom.
// The caller should wait for some blocks to complete before the funds will be accessible.
func GetAndFundTestUserWithMnemonic(
	ctx context.Context,
	keyNamePrefix, mnemonic string,
	amount math.Int,
	chain ibc.Chain,
) (ibc.Wallet, error) {
	chainCfg := chain.Config()
	keyName := fmt.Sprintf("%s-%s-%s", keyNamePrefix, chainCfg.ChainID, dockerutil.RandLowerCaseLetterString(3))
	user, err := chain.BuildWallet(ctx, keyName, mnemonic)
	if err != nil {
		return nil, fmt.Errorf("failed to get source user wallet: %w", err)
	}

	decimalPow := math.NewIntWithDecimal(1, int(*chainCfg.CoinDecimals))

	err = chain.SendFunds(ctx, FaucetAccountKeyName, ibc.WalletAmount{
		Address: user.FormattedAddress(),
		Amount:  amount.Mul(decimalPow),
		Denom:   chainCfg.Denom,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get funds from faucet: %w", err)
	}
	return user, nil
}

// GetAndFundTestUsers generates and funds chain users with the native chain denom.
// The caller should wait for some blocks to complete before the funds will be accessible.
// `amount` here is number of coins, not the amount in the smallest unit of the coin. The amount then be multiplied by 10^decimals for each chain.
func GetAndFundTestUsers(
	t *testing.T,
	ctx context.Context,
	keyNamePrefix string,
	amount math.Int,
	chains ...ibc.Chain,
) []ibc.Wallet {
	users := make([]ibc.Wallet, len(chains))
	var eg errgroup.Group
	for i, chain := range chains {
		i := i
		chain := chain
		eg.Go(func() error {
			user, err := GetAndFundTestUserWithMnemonic(ctx, keyNamePrefix, "", amount, chain)
			if err != nil {
				return err
			}
			users[i] = user
			return nil
		})
	}
	require.NoError(t, eg.Wait())

	// TODO(nix 05-17-2022): Map with generics once using go 1.18
	chainHeights := make([]testutil.ChainHeighter, len(chains))
	for i := range chains {
		chainHeights[i] = chains[i]
	}
	return users
}
