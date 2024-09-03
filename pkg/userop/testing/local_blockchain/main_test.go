package local_blockchain

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/layer-3/clearsync/pkg/signer"
	"github.com/layer-3/clearsync/pkg/smart_wallet"
	"github.com/layer-3/clearsync/pkg/userop"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func setLogLevel(level slog.Level) {
	lvl := new(slog.LevelVar)
	lvl.Set(level)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	}))

	slog.SetDefault(logger)
}

func TestSimulatedRPC(t *testing.T) {
	setLogLevel(slog.LevelDebug)
	ctx := context.Background()

	// 1. Start a local Ethereum node
	for i := 0; i < 3; i++ { // starting multiple nodes to test reusing existing nodes
		ethNode := NewEthNode(ctx, t)
		slog.Info("connecting to Ethereum node", "rpcURL", ethNode.LocalURL.String())
	}
	node := NewEthNode(ctx, t)
	slog.Info("connecting to Ethereum node", "rpcURL", node.LocalURL.String())

	// 2. Deploy the required contracts
	addresses := SetupContracts(ctx, t, node)

	// 3. Start the bundler
	for i := 0; i < 3; i++ { // starting multiple bundlers to test reusing existing bundlers
		bundlerURL := NewBundler(ctx, t, node, addresses.EntryPoint)
		slog.Info("connecting to bundler", "bundlerURL", bundlerURL.String())
	}
	bundlerURL := *NewBundler(ctx, t, node, addresses.EntryPoint)

	// 4. Build client
	client := buildClient(t, node.LocalURL, bundlerURL, addresses)

	// 5. Create and fund smart account
	eoa, receiver, swAddress := setupAccounts(ctx, t, client, node)

	// 6. Submit user operation
	signer := userop.SignerForKernel(signer.NewLocalSigner(eoa.PrivateKey))
	transferAmount := decimal.NewFromInt(1 /* 1 wei */).BigInt()
	calls := smart_wallet.Calls{{To: receiver.Address, Value: transferAmount}}
	params := &userop.WalletDeploymentOpts{Index: decimal.Zero, Owner: eoa.Address}
	op, err := client.NewUserOp(ctx, swAddress, signer, calls, params, nil)
	require.NoError(t, err, "failed to create new user operation")
	slog.Info("ready to send", "userop", op)

	done, err := client.SendUserOp(ctx, op)
	require.NoError(t, err, "failed to send user operation")

	receipt := <-done
	slog.Info("transaction mined", "receipt", receipt)
	require.True(t, receipt.Success)

	receiverBalance, err := node.Client.BalanceAt(ctx, receiver.Address, nil)
	require.NoError(t, err, "failed to fetch receiver new balance")
	require.Equal(t, transferAmount, receiverBalance, "new balance should equal the transfer amount")
}
