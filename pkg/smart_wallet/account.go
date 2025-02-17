package smart_wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/shopspring/decimal"
)

var (
	res, ok                  = entryPointABI.Errors["SenderAddressResult"]
	senderAddressResultError = mustTrue(res, ok)
)

func IsAccountDeployed(ctx context.Context, provider ethereum.ChainStateReader, swAddress common.Address) (bool, error) {
	byteCode, err := provider.CodeAt(ctx, swAddress, nil)
	if err != nil {
		return false, fmt.Errorf("failed to check if smart account is already deployed: %w", err)
	}

	// assume that the smart account is deployed if it has non-zero byte code
	return len(byteCode) != 0, nil
}

func GetAccountAddress(ctx context.Context, provider ethereum.ContractCaller, config Config, entryPointAddress, owner common.Address, index decimal.Decimal) (common.Address, error) {
	initCode, err := GetInitCode(config, owner, index)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to build initCode middleware: %w", err)
	}

	// calculate the smart wallet address that will be generated by the entry point
	// See https://github.com/eth-infinitism/account-abstraction/blob/v0.6.0/contracts/core/EntryPoint.sol#L356
	getSenderAddressData, err := entryPointABI.Pack("getSenderAddress", initCode)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to pack getSenderAddress data: %w", err)
	}

	msg := ethereum.CallMsg{
		To:   &entryPointAddress,
		Data: getSenderAddressData,
	}

	// this call must always revert (see EntryPoint contract), so we expect an error
	_, err = provider.CallContract(ctx, msg, nil)
	if err == nil {
		return common.Address{}, fmt.Errorf("'getSenderAddress' call returned no error, but expected one")
	}

	var scError rpc.DataError
	if ok := errors.As(err, &scError); !ok {
		return common.Address{}, fmt.Errorf("unexpected error type '%T' containing message %w)", err, err)
	}
	errorData, ok := scError.ErrorData().(string)
	if !ok {
		return common.Address{}, fmt.Errorf("could not unpack error data: unexpected error data (%+v) type '%T'", scError.ErrorData(), scError.ErrorData())
	}

	// check if the error signature is correct
	if id := senderAddressResultError.ID.String(); errorData[0:10] != id[0:10] {
		return common.Address{}, fmt.Errorf("'getSenderAddress' unexpected error signature: %s", errorData[0:10])
	}

	// check if the error data has the correct length
	if len(errorData) < 74 {
		return common.Address{}, fmt.Errorf("'getSenderAddress' revert data expected to have length of 74, but got: %d", len(errorData))
	}

	swAddress := common.HexToAddress(errorData[34:])
	if swAddress == (common.Address{}) {
		return common.Address{}, fmt.Errorf("'getSenderAddress' returned zero address")
	}

	return swAddress, nil
}
