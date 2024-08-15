package cosmos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkmath "cosmossdk.io/math"
	abcitypes "github.com/cometbft/cometbft/abci/types"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	bankTypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	paramsutils "github.com/cosmos/cosmos-sdk/x/params/client/utils"
	cosmosproto "github.com/cosmos/gogoproto/proto"
	chanTypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
	ccvclient "github.com/cosmos/interchain-security/v3/x/ccv/provider/client"
	dockertypes "github.com/docker/docker/api/types"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	wasmtypes "github.com/strangelove-ventures/interchaintest/v7/chain/cosmos/08-wasm-types"
	"github.com/strangelove-ventures/interchaintest/v7/chain/internal/tendermint"
	"github.com/strangelove-ventures/interchaintest/v7/dockerutil"
	"github.com/strangelove-ventures/interchaintest/v7/ibc"
	"github.com/strangelove-ventures/interchaintest/v7/internal/blockdb"
	"github.com/strangelove-ventures/interchaintest/v7/testutil"
	etherminthd "github.com/titantkx/ethermint/crypto/hd"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	defaultUpgradePath             = []string{"upgrade", "upgradedIBCState"}
	DefaultProviderUnbondingPeriod = 504 * time.Hour
)

// CosmosChain is a local docker testnet for a Cosmos SDK chain.
// Implements the ibc.Chain interface.
type CosmosChain struct {
	testName      string
	cfg           ibc.ChainConfig
	NumValidators int
	numFullNodes  int
	Validators    ChainNodes
	FullNodes     ChainNodes
	Provider      *CosmosChain
	Consumers     []*CosmosChain

	// Additional processes that need to be run on a per-chain basis.
	Sidecars SidecarProcesses

	log      *zap.Logger
	keyring  keyring.Keyring
	findTxMu sync.Mutex
}

func NewCosmosHeighlinerChainConfig(name string,
	binary string,
	bech32Prefix string,
	denom string,
	gasPrices string,
	gasAdjustment float64,
	trustingPeriod string,
	noHostMount bool,
) ibc.ChainConfig {
	return ibc.ChainConfig{
		Type:           "cosmos",
		Name:           name,
		Bech32Prefix:   bech32Prefix,
		Denom:          denom,
		GasPrices:      gasPrices,
		GasAdjustment:  gasAdjustment,
		TrustingPeriod: trustingPeriod,
		NoHostMount:    noHostMount,
		Images: []ibc.DockerImage{
			{
				Repository: fmt.Sprintf("ghcr.io/strangelove-ventures/heighliner/%s", name),
				UidGid:     dockerutil.GetHeighlinerUserString(),
			},
		},
		Bin: binary,
	}
}

func NewCosmosChain(testName string, chainConfig ibc.ChainConfig, numValidators int, numFullNodes int, log *zap.Logger) *CosmosChain {
	if chainConfig.EncodingConfig == nil {
		cfg := DefaultEncoding()
		chainConfig.EncodingConfig = &cfg
	}

	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)
	kr := keyring.NewInMemory(cdc, etherminthd.EthSecp256k1Option())

	return &CosmosChain{
		testName:      testName,
		cfg:           chainConfig,
		NumValidators: numValidators,
		numFullNodes:  numFullNodes,
		log:           log,
		keyring:       kr,
	}
}

// Nodes returns all nodes, including validators and fullnodes.
func (c *CosmosChain) Nodes() ChainNodes {
	return append(c.Validators, c.FullNodes...)
}

// AddFullNodes adds new fullnodes to the network, peering with the existing nodes.
func (c *CosmosChain) AddFullNodes(ctx context.Context, configFileOverrides map[string]any, inc int) error {
	// Get peer string for existing nodes
	peers := c.Nodes().PeerString(ctx)

	// Get genesis.json
	genbz, err := c.Validators[0].GenesisFileContent(ctx)
	if err != nil {
		return err
	}

	prevCount := c.numFullNodes
	c.numFullNodes += inc
	if err := c.initializeChainNodes(ctx, c.testName, c.getFullNode().DockerClient, c.getFullNode().NetworkID); err != nil {
		return err
	}

	var eg errgroup.Group
	for i := prevCount; i < c.numFullNodes; i++ {
		i := i
		eg.Go(func() error {
			fn := c.FullNodes[i]
			if err := fn.InitFullNodeFiles(ctx); err != nil {
				return err
			}
			if err := fn.SetPeers(ctx, peers); err != nil {
				return err
			}
			if err := fn.OverwriteGenesisFile(ctx, genbz); err != nil {
				return err
			}
			for configFile, modifiedConfig := range configFileOverrides {
				modifiedToml, ok := modifiedConfig.(testutil.Toml)
				if !ok {
					return fmt.Errorf("Provided toml override for file %s is of type (%T). Expected (DecodedToml)", configFile, modifiedConfig)
				}
				if err := testutil.ModifyTomlConfigFile(
					ctx,
					fn.logger(),
					fn.DockerClient,
					fn.TestName,
					fn.VolumeName,
					configFile,
					modifiedToml,
				); err != nil {
					return err
				}
			}
			if err := fn.CreateNodeContainer(ctx); err != nil {
				return err
			}
			return fn.StartContainer(ctx)
		})
	}
	return eg.Wait()
}

// Implements Chain interface
func (c *CosmosChain) Config() ibc.ChainConfig {
	return c.cfg
}

// Implements Chain interface
func (c *CosmosChain) Initialize(ctx context.Context, testName string, cli *client.Client, networkID string) error {
	if err := c.initializeSidecars(ctx, testName, cli, networkID); err != nil {
		return err
	}
	return c.initializeChainNodes(ctx, testName, cli, networkID)
}

func (c *CosmosChain) getFullNode() *ChainNode {
	c.findTxMu.Lock()
	defer c.findTxMu.Unlock()
	if len(c.FullNodes) > 0 {
		// use first full node
		return c.FullNodes[0]
	}
	// use first validator
	return c.Validators[0]
}

func (c *CosmosChain) GetNode() *ChainNode {
	return c.Validators[0]
}

// Exec implements ibc.Chain.
func (c *CosmosChain) Exec(ctx context.Context, cmd []string, env []string) (stdout, stderr []byte, err error) {
	return c.getFullNode().Exec(ctx, cmd, env)
}

// Implements Chain interface
func (c *CosmosChain) GetRPCAddress() string {
	return fmt.Sprintf("http://%s:26657", c.getFullNode().HostName())
}

// Implements Chain interface
func (c *CosmosChain) GetAPIAddress() string {
	return fmt.Sprintf("http://%s:1317", c.getFullNode().HostName())
}

// Implements Chain interface
func (c *CosmosChain) GetGRPCAddress() string {
	return fmt.Sprintf("%s:9090", c.getFullNode().HostName())
}

// GetHostRPCAddress returns the address of the RPC server accessible by the host.
// This will not return a valid address until the chain has been started.
func (c *CosmosChain) GetHostRPCAddress() string {
	return "http://" + c.getFullNode().hostRPCPort
}

// GetHostAPIAddress returns the address of the REST API server accessible by the host.
// This will not return a valid address until the chain has been started.
func (c *CosmosChain) GetHostAPIAddress() string {
	return "http://" + c.getFullNode().hostAPIPort
}

// GetHostGRPCAddress returns the address of the gRPC server accessible by the host.
// This will not return a valid address until the chain has been started.
func (c *CosmosChain) GetHostGRPCAddress() string {
	return c.getFullNode().hostGRPCPort
}

// HomeDir implements ibc.Chain.
func (c *CosmosChain) HomeDir() string {
	return c.getFullNode().HomeDir()
}

// Implements Chain interface
func (c *CosmosChain) CreateKey(ctx context.Context, keyName string) error {
	return c.getFullNode().CreateKey(ctx, keyName)
}

// Implements Chain interface
func (c *CosmosChain) RecoverKey(ctx context.Context, keyName, mnemonic string) error {
	return c.getFullNode().RecoverKey(ctx, keyName, mnemonic)
}

// Implements Chain interface
func (c *CosmosChain) GetAddress(ctx context.Context, keyName string) ([]byte, error) {
	b32Addr, err := c.getFullNode().AccountKeyBech32(ctx, keyName)
	if err != nil {
		return nil, err
	}

	return types.GetFromBech32(b32Addr, c.Config().Bech32Prefix)
}

// BuildWallet will return a Cosmos wallet
// If mnemonic != "", it will restore using that mnemonic
// If mnemonic == "", it will create a new key
func (c *CosmosChain) BuildWallet(ctx context.Context, keyName string, mnemonic string) (ibc.Wallet, error) {
	if mnemonic != "" {
		if err := c.RecoverKey(ctx, keyName, mnemonic); err != nil {
			return nil, fmt.Errorf("failed to recover key with name %q on chain %s: %w", keyName, c.cfg.Name, err)
		}
	} else {
		if err := c.CreateKey(ctx, keyName); err != nil {
			return nil, fmt.Errorf("failed to create key with name %q on chain %s: %w", keyName, c.cfg.Name, err)
		}
	}

	addrBytes, err := c.GetAddress(ctx, keyName)
	if err != nil {
		return nil, fmt.Errorf("failed to get account address for key %q on chain %s: %w", keyName, c.cfg.Name, err)
	}

	return NewWallet(keyName, addrBytes, mnemonic, c.cfg), nil
}

// BuildRelayerWallet will return a Cosmos wallet populated with the mnemonic so that the wallet can
// be restored in the relayer node using the mnemonic. After it is built, that address is included in
// genesis with some funds.
func (c *CosmosChain) BuildRelayerWallet(ctx context.Context, keyName string) (ibc.Wallet, error) {
	coinType, err := strconv.ParseUint(c.cfg.CoinType, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid coin type: %w", err)
	}

	var algo keyring.SignatureAlgo
	switch c.cfg.CoinType {
	case "60":
		algo = etherminthd.EthSecp256k1
	default:
		algo = hd.Secp256k1
	}

	info, mnemonic, err := c.keyring.NewMnemonic(
		keyName,
		keyring.English,
		hd.CreateHDPath(uint32(coinType), 0, 0).String(),
		"", // Empty passphrase.
		algo,
	)
	fmt.Printf("Created key %s with mnemonic %s\n", keyName, mnemonic)
	if err != nil {
		return nil, fmt.Errorf("failed to create mnemonic: %w", err)
	}

	addrBytes, err := info.GetAddress()
	fmt.Printf("Created key %s with address %s\n", keyName, addrBytes.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	return NewWallet(keyName, addrBytes, mnemonic, c.cfg), nil
}

// Implements Chain interface
func (c *CosmosChain) SendFunds(ctx context.Context, keyName string, amount ibc.WalletAmount) error {
	return c.getFullNode().SendFunds(ctx, keyName, amount)
}

func (c *CosmosChain) getFee(events []abcitypes.Event) (sdkmath.Int, error) {
	fee, _ := tendermint.AttributeValue(events, "tx", "fee")
	refund, hasRefund := tendermint.AttributeValue(events, "refund", "amount")

	feeCoin, err := sdk.ParseCoinNormalized(fee)
	if err != nil {
		return sdkmath.Int{}, fmt.Errorf("failed to parse fee coin %s: %w", fee, err)
	}

	if hasRefund {
		refundCoin, err := sdk.ParseCoinNormalized(refund)
		if err != nil {
			return sdkmath.Int{}, fmt.Errorf("failed to parse refund coin %s: %w", refund, err)
		}
		return feeCoin.Amount.Sub(refundCoin.Amount), nil
	} else {
		return feeCoin.Amount, nil
	}
}

// Implements Chain interface
func (c *CosmosChain) SendIBCTransfer(
	ctx context.Context,
	channelID string,
	keyName string,
	amount ibc.WalletAmount,
	options ibc.TransferOptions,
) (tx ibc.Tx, _ error) {
	txHash, err := c.getFullNode().SendIBCTransfer(ctx, channelID, keyName, amount, options)
	if err != nil {
		return tx, fmt.Errorf("send ibc transfer: %w", err)
	}
	txResp, err := c.GetTransaction(txHash)
	if err != nil {
		return tx, fmt.Errorf("failed to get transaction %s: %w", txHash, err)
	}
	if txResp.Code != 0 {
		return tx, fmt.Errorf("error in transaction (code: %d): %s", txResp.Code, txResp.RawLog)
	}
	tx.Height = txResp.Height
	tx.TxHash = txHash

	if c.Config().UseGasUsed {
		tx.GasSpent = txResp.GasUsed
	} else {
		// In cosmos, user is charged for entire gas requested, not the actual gas used.
		tx.GasSpent = txResp.GasWanted
	}

	events := txResp.Events

	tx.Fee, _ = c.getFee(events)

	const evType = "send_packet"

	var (
		seq, _           = tendermint.AttributeValue(events, evType, "packet_sequence")
		srcPort, _       = tendermint.AttributeValue(events, evType, "packet_src_port")
		srcChan, _       = tendermint.AttributeValue(events, evType, "packet_src_channel")
		dstPort, _       = tendermint.AttributeValue(events, evType, "packet_dst_port")
		dstChan, _       = tendermint.AttributeValue(events, evType, "packet_dst_channel")
		timeoutHeight, _ = tendermint.AttributeValue(events, evType, "packet_timeout_height")
		timeoutTs, _     = tendermint.AttributeValue(events, evType, "packet_timeout_timestamp")
		data, _          = tendermint.AttributeValue(events, evType, "packet_data")
	)
	tx.Packet.SourcePort = srcPort
	tx.Packet.SourceChannel = srcChan
	tx.Packet.DestPort = dstPort
	tx.Packet.DestChannel = dstChan
	tx.Packet.TimeoutHeight = timeoutHeight
	tx.Packet.Data = []byte(data)

	seqNum, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return tx, fmt.Errorf("invalid packet sequence from events %s: %w", seq, err)
	}
	tx.Packet.Sequence = seqNum

	timeoutNano, err := strconv.ParseUint(timeoutTs, 10, 64)
	if err != nil {
		return tx, fmt.Errorf("invalid packet timestamp timeout %s: %w", timeoutTs, err)
	}
	tx.Packet.TimeoutTimestamp = ibc.Nanoseconds(timeoutNano)

	return tx, nil
}

// GetGovernanceAddress performs a query to get the address of the chain's x/gov module
func (c *CosmosChain) GetGovernanceAddress(ctx context.Context) (string, error) {
	return c.GetModuleAddress(ctx, govtypes.ModuleName)
}

// GetModuleAddress performs a query to get the address of the specified chain module
func (c *CosmosChain) GetModuleAddress(ctx context.Context, moduleName string) (string, error) {
	return c.getFullNode().GetModuleAddress(ctx, moduleName)
}

// QueryProposal returns the state and details of a governance proposal.
func (c *CosmosChain) QueryProposal(ctx context.Context, proposalID string) (*ProposalResponse, error) {
	return c.getFullNode().QueryProposal(ctx, proposalID)
}

// RegisterICA will attempt to register an interchain account on the given counterparty chain.
func (c *CosmosChain) RegisterICA(ctx context.Context, keyName string, connectionID string) (string, error) {
	return c.getFullNode().RegisterICA(ctx, keyName, connectionID)
}

// QueryICA will query for an interchain account controlled by the given address on the counterparty chain.
func (c *CosmosChain) QueryICAAddress(ctx context.Context, connectionID, address string) (string, error) {
	return c.getFullNode().QueryICA(ctx, connectionID, address)
}

// SendICATx sends an interchain account transaction for a specified address and sends it to the respective
// interchain account on the counterparty chain.
func (c *CosmosChain) SendICATx(ctx context.Context, keyName, connectionID string, msgs []sdk.Msg, icaTxMemo string) (string, error) {
	node := c.getFullNode()
	registry := node.Chain.Config().EncodingConfig.InterfaceRegistry
	return node.SendICATx(ctx, keyName, connectionID, registry, msgs, icaTxMemo)
}

// PushNewWasmClientProposal submits a new wasm client governance proposal to the chain
func (c *CosmosChain) PushNewWasmClientProposal(ctx context.Context, keyName string, fileName string, prop TxProposalv1) (TxProposal, string, error) {
	tx := TxProposal{}
	content, err := os.ReadFile(fileName)
	if err != nil {
		return tx, "", err
	}
	codeHashByte32 := sha256.Sum256(content)
	codeHash := hex.EncodeToString(codeHashByte32[:])
	content, err = testutil.GzipIt(content)
	if err != nil {
		return tx, "", err
	}
	message := wasmtypes.MsgStoreCode{
		Signer: types.MustBech32ifyAddressBytes(c.cfg.Bech32Prefix, authtypes.NewModuleAddress(govtypes.ModuleName)),
		Code:   content,
	}
	msg, err := c.cfg.EncodingConfig.Codec.MarshalInterfaceJSON(&message)
	if err != nil {
		return tx, "", err
	}
	prop.Messages = append(prop.Messages, msg)
	txHash, err := c.getFullNode().SubmitProposal(ctx, keyName, prop)
	if err != nil {
		return tx, "", fmt.Errorf("failed to submit wasm client proposal: %w", err)
	}
	tx, err = c.txProposal(txHash)
	return tx, codeHash, err
}

// UpgradeProposal submits a software-upgrade governance proposal to the chain.
func (c *CosmosChain) UpgradeProposal(ctx context.Context, keyName string, prop SoftwareUpgradeProposal) (tx TxProposal, _ error) {
	txHash, err := c.getFullNode().UpgradeProposal(ctx, keyName, prop)
	if err != nil {
		return tx, fmt.Errorf("failed to submit upgrade proposal: %w", err)
	}
	return c.txProposal(txHash)
}

// SubmitProposal submits a gov v1 proposal to the chain.
func (c *CosmosChain) SubmitProposal(ctx context.Context, keyName string, prop TxProposalv1) (tx TxProposal, _ error) {
	txHash, err := c.getFullNode().SubmitProposal(ctx, keyName, prop)
	if err != nil {
		return tx, fmt.Errorf("failed to submit gov v1 proposal: %w", err)
	}
	return c.txProposal(txHash)
}

// Build a gov v1 proposal type.
func (c *CosmosChain) BuildProposal(messages []cosmosproto.Message, title, summary, metadata, depositStr string) (TxProposalv1, error) {
	var propType TxProposalv1
	rawMsgs := make([]json.RawMessage, len(messages))

	for i, msg := range messages {
		msg, err := c.Config().EncodingConfig.Codec.MarshalInterfaceJSON(msg)
		if err != nil {
			return propType, err
		}
		rawMsgs[i] = msg
	}

	propType = TxProposalv1{
		Messages: rawMsgs,
		Metadata: metadata,
		Deposit:  depositStr,
		Title:    title,
		Summary:  summary,
	}

	return propType, nil
}

// TextProposal submits a text governance proposal to the chain.
func (c *CosmosChain) TextProposal(ctx context.Context, keyName string, prop TextProposal) (tx TxProposal, _ error) {
	txHash, err := c.getFullNode().TextProposal(ctx, keyName, prop)
	if err != nil {
		return tx, fmt.Errorf("failed to submit upgrade proposal: %w", err)
	}
	return c.txProposal(txHash)
}

// ParamChangeProposal submits a param change proposal to the chain, signed by keyName.
func (c *CosmosChain) ParamChangeProposal(ctx context.Context, keyName string, prop *paramsutils.ParamChangeProposalJSON) (tx TxProposal, _ error) {
	txHash, err := c.getFullNode().ParamChangeProposal(ctx, keyName, prop)
	if err != nil {
		return tx, fmt.Errorf("failed to submit param change proposal: %w", err)
	}

	return c.txProposal(txHash)
}

// QueryParam returns the param state of a given key.
func (c *CosmosChain) QueryParam(ctx context.Context, subspace, key string) (*ParamChange, error) {
	return c.getFullNode().QueryParam(ctx, subspace, key)
}

// QueryBankMetadata returns the metadata of a given token denomination.
func (c *CosmosChain) QueryBankMetadata(ctx context.Context, denom string) (*BankMetaData, error) {
	return c.getFullNode().QueryBankMetadata(ctx, denom)
}

// ConsumerAdditionProposal submits a legacy governance proposal to add a consumer to the chain.
func (c *CosmosChain) ConsumerAdditionProposal(ctx context.Context, keyName string, prop ccvclient.ConsumerAdditionProposalJSON) (tx TxProposal, _ error) {
	txHash, err := c.getFullNode().ConsumerAdditionProposal(ctx, keyName, prop)
	if err != nil {
		return tx, fmt.Errorf("failed to submit consumer addition proposal: %w", err)
	}
	return c.txProposal(txHash)
}

func (c *CosmosChain) txProposal(txHash string) (tx TxProposal, _ error) {
	txResp, err := c.GetTransaction(txHash)
	if err != nil {
		return tx, fmt.Errorf("failed to get transaction %s: %w", txHash, err)
	}
	tx.Height = txResp.Height
	tx.TxHash = txHash
	if c.Config().UseGasUsed {
		tx.GasSpent = txResp.GasUsed
	} else {
		// In cosmos, user is charged for entire gas requested, not the actual gas used.
		tx.GasSpent = txResp.GasWanted
	}
	events := txResp.Events
	tx.Fee, _ = c.getFee(events)

	tx.DepositAmount, _ = tendermint.AttributeValue(events, "proposal_deposit", "amount")

	evtSubmitProp := "submit_proposal"
	tx.ProposalID, _ = tendermint.AttributeValue(events, evtSubmitProp, "proposal_id")
	tx.ProposalType, _ = tendermint.AttributeValue(events, evtSubmitProp, "proposal_type")

	return tx, nil
}

// StoreContract takes a file path to smart contract and stores it on-chain. Returns the contracts code id.
func (c *CosmosChain) StoreContract(ctx context.Context, keyName string, fileName string, extraExecTxArgs ...string) (string, error) {
	return c.getFullNode().StoreContract(ctx, keyName, fileName, extraExecTxArgs...)
}

// InstantiateContract takes a code id for a smart contract and initialization message and returns the instantiated contract address.
func (c *CosmosChain) InstantiateContract(ctx context.Context, keyName string, codeID string, initMessage string, needsNoAdminFlag bool, extraExecTxArgs ...string) (string, error) {
	return c.getFullNode().InstantiateContract(ctx, keyName, codeID, initMessage, needsNoAdminFlag, extraExecTxArgs...)
}

// ExecuteContract executes a contract transaction with a message using it's address.
func (c *CosmosChain) ExecuteContract(ctx context.Context, keyName string, contractAddress string, message string, extraExecTxArgs ...string) (res *types.TxResponse, err error) {
	return c.getFullNode().ExecuteContract(ctx, keyName, contractAddress, message, extraExecTxArgs...)
}

// MigrateContract performs contract migration
func (c *CosmosChain) MigrateContract(ctx context.Context, keyName string, contractAddress string, codeID string, message string, extraExecTxArgs ...string) (res *types.TxResponse, err error) {
	return c.getFullNode().MigrateContract(ctx, keyName, contractAddress, codeID, message, extraExecTxArgs...)
}

// QueryContract performs a smart query, taking in a query struct and returning a error with the response struct populated.
func (c *CosmosChain) QueryContract(ctx context.Context, contractAddress string, query any, response any) error {
	return c.getFullNode().QueryContract(ctx, contractAddress, query, response)
}

// DumpContractState dumps the state of a contract at a block height.
func (c *CosmosChain) DumpContractState(ctx context.Context, contractAddress string, height int64) (*DumpContractStateResponse, error) {
	return c.getFullNode().DumpContractState(ctx, contractAddress, height)
}

// StoreClientContract takes a file path to a client smart contract and stores it on-chain. Returns the contracts code id.
func (c *CosmosChain) StoreClientContract(ctx context.Context, keyName string, fileName string, extraExecTxArgs ...string) (string, error) {
	return c.getFullNode().StoreClientContract(ctx, keyName, fileName, extraExecTxArgs...)
}

// QueryClientContractCode performs a query with the contract codeHash as the input and code as the output
func (c *CosmosChain) QueryClientContractCode(ctx context.Context, codeHash string, response any) error {
	return c.getFullNode().QueryClientContractCode(ctx, codeHash, response)
}

// ExportState exports the chain state at specific height.
// Implements Chain interface
func (c *CosmosChain) ExportState(ctx context.Context, height int64) (string, error) {
	return c.getFullNode().ExportState(ctx, height)
}

// AllBalances fetches an account address's balance for all denoms it holds
func (c *CosmosChain) AllBalances(ctx context.Context, address string) (types.Coins, error) {
	params := bankTypes.QueryAllBalancesRequest{Address: address}
	grpcAddress := c.getFullNode().hostGRPCPort
	conn, err := grpc.Dial(grpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	queryClient := bankTypes.NewQueryClient(conn)
	res, err := queryClient.AllBalances(ctx, &params)
	if err != nil {
		return nil, err
	}

	return res.GetBalances(), nil
}

func (c *CosmosChain) GetTransaction(txhash string) (*types.TxResponse, error) {
	fn := c.getFullNode()
	return fn.GetTransaction(fn.CliContext(), txhash)
}

func (c *CosmosChain) GetGasFeesInNativeDenom(gasPaid int64) int64 {
	gasPrice, _ := strconv.ParseFloat(strings.Replace(c.cfg.GasPrices, c.cfg.Denom, "", 1), 64)
	fees := float64(gasPaid) * gasPrice
	return int64(fees)
}

func (c *CosmosChain) UpgradeVersion(ctx context.Context, cli *client.Client, containerRepo, version string) {
	c.cfg.Images[0].Version = version
	for _, n := range c.Validators {
		n.Image.Version = version
		n.Image.Repository = containerRepo
	}
	for _, n := range c.FullNodes {
		n.Image.Version = version
		n.Image.Repository = containerRepo
	}
	c.pullImages(ctx, cli)
}

func (c *CosmosChain) pullImages(ctx context.Context, cli *client.Client) {
	for _, image := range c.Config().Images {
		rc, err := cli.ImagePull(
			ctx,
			image.Repository+":"+image.Version,
			dockertypes.ImagePullOptions{},
		)
		if err != nil {
			c.log.Error("Failed to pull image",
				zap.Error(err),
				zap.String("repository", image.Repository),
				zap.String("tag", image.Version),
			)
		} else {
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
		}
	}
}

// NewChainNode constructs a new cosmos chain node with a docker volume.
func (c *CosmosChain) NewChainNode(
	ctx context.Context,
	testName string,
	cli *client.Client,
	networkID string,
	image ibc.DockerImage,
	validator bool,
	index int,
) (*ChainNode, error) {
	// Construct the ChainNode first so we can access its name.
	// The ChainNode's VolumeName cannot be set until after we create the volume.
	tn := NewChainNode(c.log, validator, c, cli, networkID, testName, image, index)

	v, err := cli.VolumeCreate(ctx, volumetypes.CreateOptions{
		Labels: map[string]string{
			dockerutil.CleanupLabel: testName,

			dockerutil.NodeOwnerLabel: tn.Name(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating volume for chain node: %w", err)
	}
	tn.VolumeName = v.Name

	if err := dockerutil.SetVolumeOwner(ctx, dockerutil.VolumeOwnerOptions{
		Log: c.log,

		Client: cli,

		VolumeName: v.Name,
		ImageRef:   image.Ref(),
		TestName:   testName,
		UidGid:     image.UidGid,
	}); err != nil {
		return nil, fmt.Errorf("set volume owner: %w", err)
	}

	for _, cfg := range c.cfg.SidecarConfigs {
		if !cfg.ValidatorProcess {
			continue
		}

		err = tn.NewSidecarProcess(ctx, cfg.PreStart, cfg.ProcessName, cli, networkID, cfg.Image, cfg.HomeDir, cfg.Ports, cfg.StartCmd, cfg.Env)
		if err != nil {
			return nil, err
		}
	}

	return tn, nil
}

// NewSidecarProcess constructs a new sidecar process with a docker volume.
func (c *CosmosChain) NewSidecarProcess(
	ctx context.Context,
	preStart bool,
	processName string,
	testName string,
	cli *client.Client,
	networkID string,
	image ibc.DockerImage,
	homeDir string,
	index int,
	ports []string,
	startCmd []string,
	env []string,
) error {
	// Construct the SidecarProcess first so we can access its name.
	// The SidecarProcess's VolumeName cannot be set until after we create the volume.
	s := NewSidecar(c.log, false, preStart, c, cli, networkID, processName, testName, image, homeDir, index, ports, startCmd, env)

	v, err := cli.VolumeCreate(ctx, volumetypes.CreateOptions{
		Labels: map[string]string{
			dockerutil.CleanupLabel:   testName,
			dockerutil.NodeOwnerLabel: s.Name(),
		},
	})
	if err != nil {
		return fmt.Errorf("creating volume for sidecar process: %w", err)
	}
	s.VolumeName = v.Name

	if err := dockerutil.SetVolumeOwner(ctx, dockerutil.VolumeOwnerOptions{
		Log: c.log,

		Client: cli,

		VolumeName: v.Name,
		ImageRef:   image.Ref(),
		TestName:   testName,
		UidGid:     image.UidGid,
	}); err != nil {
		return fmt.Errorf("set volume owner: %w", err)
	}

	c.Sidecars = append(c.Sidecars, s)

	return nil
}

// creates the test node objects required for bootstrapping tests
func (c *CosmosChain) initializeChainNodes(
	ctx context.Context,
	testName string,
	cli *client.Client,
	networkID string,
) error {
	chainCfg := c.Config()
	c.pullImages(ctx, cli)
	image := chainCfg.Images[0]

	newVals := make(ChainNodes, c.NumValidators)
	copy(newVals, c.Validators)
	newFullNodes := make(ChainNodes, c.numFullNodes)
	copy(newFullNodes, c.FullNodes)

	eg, egCtx := errgroup.WithContext(ctx)
	for i := len(c.Validators); i < c.NumValidators; i++ {
		i := i
		eg.Go(func() error {
			val, err := c.NewChainNode(egCtx, testName, cli, networkID, image, true, i)
			if err != nil {
				return err
			}
			newVals[i] = val
			return nil
		})
	}
	for i := len(c.FullNodes); i < c.numFullNodes; i++ {
		i := i
		eg.Go(func() error {
			fn, err := c.NewChainNode(egCtx, testName, cli, networkID, image, false, i)
			if err != nil {
				return err
			}
			newFullNodes[i] = fn
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	c.findTxMu.Lock()
	defer c.findTxMu.Unlock()
	c.Validators = newVals
	c.FullNodes = newFullNodes
	return nil
}

// initializeSidecars creates the sidecar processes that exist at the chain level.
func (c *CosmosChain) initializeSidecars(
	ctx context.Context,
	testName string,
	cli *client.Client,
	networkID string,
) error {
	eg, egCtx := errgroup.WithContext(ctx)
	for i, cfg := range c.cfg.SidecarConfigs {
		i := i
		cfg := cfg

		if cfg.ValidatorProcess {
			continue
		}

		eg.Go(func() error {
			err := c.NewSidecarProcess(egCtx, cfg.PreStart, cfg.ProcessName, testName, cli, networkID, cfg.Image, cfg.HomeDir, i, cfg.Ports, cfg.StartCmd, cfg.Env)
			if err != nil {
				return err
			}
			return nil
		})

	}
	if err := eg.Wait(); err != nil {
		return err
	}
	return nil
}

type GenesisValidatorPubKey struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}
type GenesisValidators struct {
	Address string                 `json:"address"`
	Name    string                 `json:"name"`
	Power   string                 `json:"power"`
	PubKey  GenesisValidatorPubKey `json:"pub_key"`
}
type GenesisFile struct {
	Validators []GenesisValidators `json:"validators"`
}

type ValidatorWithIntPower struct {
	Address      string
	Power        int64
	PubKeyBase64 string
}

// Bootstraps the chain and starts it from genesis
func (c *CosmosChain) Start(testName string, ctx context.Context, additionalGenesisWallets ...ibc.WalletAmount) error {
	if c.cfg.InterchainSecurityConfig.ConsumerCopyProviderKey != nil && c.Provider == nil {
		return fmt.Errorf("don't set ConsumerCopyProviderKey if it's not a consumer chain")
	}

	chainCfg := c.Config()

	decimalPow := int64(math.Pow10(int(*chainCfg.CoinDecimals)))

	genesisAmounts := make([][]types.Coin, len(c.Validators))
	genesisSelfDelegation := make([]types.Coin, len(c.Validators))

	for i := range c.Validators {
		genesisAmounts[i] = []types.Coin{{Amount: sdkmath.NewInt(10_000_000).MulRaw(decimalPow), Denom: chainCfg.Denom}}
		genesisSelfDelegation[i] = types.Coin{Amount: sdkmath.NewInt(5_000_000).MulRaw(decimalPow), Denom: chainCfg.Denom}
		if chainCfg.ModifyGenesisAmounts != nil {
			amount, selfDelegation := chainCfg.ModifyGenesisAmounts(i)
			genesisAmounts[i] = []types.Coin{amount}
			genesisSelfDelegation[i] = selfDelegation
		}
	}

	configFileOverrides := chainCfg.ConfigFileOverrides

	eg := new(errgroup.Group)
	// Initialize config and sign gentx for each validator.
	for i, v := range c.Validators {
		v := v
		i := i
		v.Validator = true
		eg.Go(func() error {
			if err := v.InitFullNodeFiles(ctx); err != nil {
				return err
			}
			for configFile, modifiedConfig := range configFileOverrides {
				modifiedToml, ok := modifiedConfig.(testutil.Toml)
				if !ok {
					return fmt.Errorf("Provided toml override for file %s is of type (%T). Expected (DecodedToml)", configFile, modifiedConfig)
				}
				if err := testutil.ModifyTomlConfigFile(
					ctx,
					v.logger(),
					v.DockerClient,
					v.TestName,
					v.VolumeName,
					configFile,
					modifiedToml,
				); err != nil {
					return err
				}
			}
			if !c.cfg.SkipGenTx {
				return v.InitValidatorGenTx(ctx, &chainCfg, genesisAmounts[i], genesisSelfDelegation[i])
			}
			return nil
		})
	}

	// Initialize config for each full node.
	for _, n := range c.FullNodes {
		n := n
		n.Validator = false
		eg.Go(func() error {
			if err := n.InitFullNodeFiles(ctx); err != nil {
				return err
			}
			for configFile, modifiedConfig := range configFileOverrides {
				modifiedToml, ok := modifiedConfig.(testutil.Toml)
				if !ok {
					return fmt.Errorf("Provided toml override for file %s is of type (%T). Expected (DecodedToml)", configFile, modifiedConfig)
				}
				if err := testutil.ModifyTomlConfigFile(
					ctx,
					n.logger(),
					n.DockerClient,
					n.TestName,
					n.VolumeName,
					configFile,
					modifiedToml,
				); err != nil {
					return err
				}
			}
			return nil
		})
	}

	// wait for this to finish
	if err := eg.Wait(); err != nil {
		return err
	}

	if c.cfg.PreGenesis != nil {
		err := c.cfg.PreGenesis(chainCfg)
		if err != nil {
			return err
		}
	}

	// for the validators we need to collect the gentxs and the accounts
	// to the first node's genesis file
	validator0 := c.Validators[0]
	for i := 1; i < len(c.Validators); i++ {
		validatorN := c.Validators[i]

		bech32, err := validatorN.AccountKeyBech32(ctx, valKey)
		if err != nil {
			return err
		}

		if err := validator0.AddGenesisAccount(ctx, bech32, genesisAmounts[0]); err != nil {
			return err
		}

		if !c.cfg.SkipGenTx {
			if err := validatorN.copyGentx(ctx, validator0); err != nil {
				return err
			}
		}
	}

	for _, wallet := range additionalGenesisWallets {
		if err := validator0.AddGenesisAccount(ctx, wallet.Address, []types.Coin{{Denom: wallet.Denom, Amount: wallet.Amount}}); err != nil {
			return err
		}
	}

	if !c.cfg.SkipGenTx {
		if err := validator0.CollectGentxs(ctx); err != nil {
			return err
		}
	}

	genbz, err := validator0.GenesisFileContent(ctx)
	if err != nil {
		return err
	}

	genbz = bytes.ReplaceAll(genbz, []byte(`"stake"`), []byte(fmt.Sprintf(`"%s"`, chainCfg.Denom)))

	if c.cfg.ModifyGenesis != nil {
		genbz, err = c.cfg.ModifyGenesis(chainCfg, genbz)
		if err != nil {
			return err
		}
	}

	// Provide EXPORT_GENESIS_FILE_PATH and EXPORT_GENESIS_CHAIN to help debug genesis file
	exportGenesis := os.Getenv("EXPORT_GENESIS_FILE_PATH")
	exportGenesisChain := os.Getenv("EXPORT_GENESIS_CHAIN")
	if exportGenesis != "" && exportGenesisChain == c.cfg.Name {
		c.log.Debug("Exporting genesis file",
			zap.String("chain", exportGenesisChain),
			zap.String("path", exportGenesis),
		)
		_ = os.WriteFile(exportGenesis, genbz, 0o600)
	}

	chainNodes := c.Nodes()

	for _, cn := range chainNodes {
		if err := cn.OverwriteGenesisFile(ctx, genbz); err != nil {
			return err
		}
	}

	c.log.Debug("Genesis file",
		zap.String("chain", c.cfg.Name),
		zap.String("genesis", string(genbz)),
	)

	if err := chainNodes.LogGenesisHashes(ctx); err != nil {
		return err
	}

	// Start any sidecar processes that should be running before the chain starts
	eg, egCtx := errgroup.WithContext(ctx)
	for _, s := range c.Sidecars {
		s := s

		err = s.containerLifecycle.Running(ctx)
		if s.preStart && err != nil {
			eg.Go(func() error {
				if err := s.CreateContainer(egCtx); err != nil {
					return err
				}

				if err := s.StartContainer(egCtx); err != nil {
					return err
				}

				return nil
			})
		}
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	eg, egCtx = errgroup.WithContext(ctx)
	for _, n := range chainNodes {
		n := n
		eg.Go(func() error {
			return n.CreateNodeContainer(egCtx)
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	peers := chainNodes.PeerString(ctx)

	eg, egCtx = errgroup.WithContext(ctx)
	for _, n := range chainNodes {
		n := n
		c.log.Info("Starting container", zap.String("container", n.Name()))
		eg.Go(func() error {
			if err := n.SetPeers(egCtx, peers); err != nil {
				return err
			}
			return n.StartContainer(egCtx)
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// Wait for 5 blocks before considering the chains "started"
	return testutil.WaitForBlocks(ctx, 5, c.getFullNode())
}

// Height implements ibc.Chain
func (c *CosmosChain) Height(ctx context.Context) (int64, error) {
	return c.getFullNode().Height(ctx)
}

// Acknowledgements implements ibc.Chain, returning all acknowledgments in block at height
func (c *CosmosChain) Acknowledgements(ctx context.Context, height int64) ([]ibc.PacketAcknowledgement, error) {
	var acks []*chanTypes.MsgAcknowledgement
	err := RangeBlockMessages(ctx, c.cfg.EncodingConfig.InterfaceRegistry, c.getFullNode().Client, height, func(msg types.Msg) bool {
		found, ok := msg.(*chanTypes.MsgAcknowledgement)
		if ok {
			acks = append(acks, found)
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("find acknowledgements at height %d: %w", height, err)
	}
	ibcAcks := make([]ibc.PacketAcknowledgement, len(acks))
	for i, ack := range acks {
		ack := ack
		ibcAcks[i] = ibc.PacketAcknowledgement{
			Acknowledgement: ack.Acknowledgement,
			Packet: ibc.Packet{
				Sequence:         ack.Packet.Sequence,
				SourcePort:       ack.Packet.SourcePort,
				SourceChannel:    ack.Packet.SourceChannel,
				DestPort:         ack.Packet.DestinationPort,
				DestChannel:      ack.Packet.DestinationChannel,
				Data:             ack.Packet.Data,
				TimeoutHeight:    ack.Packet.TimeoutHeight.String(),
				TimeoutTimestamp: ibc.Nanoseconds(ack.Packet.TimeoutTimestamp),
			},
		}
	}
	return ibcAcks, nil
}

// Timeouts implements ibc.Chain, returning all timeouts in block at height
func (c *CosmosChain) Timeouts(ctx context.Context, height int64) ([]ibc.PacketTimeout, error) {
	var timeouts []*chanTypes.MsgTimeout
	err := RangeBlockMessages(ctx, c.cfg.EncodingConfig.InterfaceRegistry, c.getFullNode().Client, height, func(msg types.Msg) bool {
		found, ok := msg.(*chanTypes.MsgTimeout)
		if ok {
			timeouts = append(timeouts, found)
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("find timeouts at height %d: %w", height, err)
	}
	ibcTimeouts := make([]ibc.PacketTimeout, len(timeouts))
	for i, ack := range timeouts {
		ack := ack
		ibcTimeouts[i] = ibc.PacketTimeout{
			Packet: ibc.Packet{
				Sequence:         ack.Packet.Sequence,
				SourcePort:       ack.Packet.SourcePort,
				SourceChannel:    ack.Packet.SourceChannel,
				DestPort:         ack.Packet.DestinationPort,
				DestChannel:      ack.Packet.DestinationChannel,
				Data:             ack.Packet.Data,
				TimeoutHeight:    ack.Packet.TimeoutHeight.String(),
				TimeoutTimestamp: ibc.Nanoseconds(ack.Packet.TimeoutTimestamp),
			},
		}
	}
	return ibcTimeouts, nil
}

// FindTxs implements blockdb.BlockSaver.
func (c *CosmosChain) FindTxs(ctx context.Context, height int64) ([]blockdb.Tx, error) {
	fn := c.getFullNode()
	c.findTxMu.Lock()
	defer c.findTxMu.Unlock()
	return fn.FindTxs(ctx, height)
}

// StopAllNodes stops and removes all long running containers (validators and full nodes)
func (c *CosmosChain) StopAllNodes(ctx context.Context) error {
	var eg errgroup.Group
	for _, n := range c.Nodes() {
		n := n
		eg.Go(func() error {
			if err := n.StopContainer(ctx); err != nil {
				return err
			}
			return n.RemoveContainer(ctx)
		})
	}
	return eg.Wait()
}

// StopAllSidecars stops and removes all long-running containers for sidecar processes.
func (c *CosmosChain) StopAllSidecars(ctx context.Context) error {
	var eg errgroup.Group
	for _, s := range c.Sidecars {
		s := s
		eg.Go(func() error {
			if err := s.StopContainer(ctx); err != nil {
				return err
			}
			return s.RemoveContainer(ctx)
		})
	}
	return eg.Wait()
}

// StartAllNodes creates and starts new containers for each node.
// Should only be used if the chain has previously been started with .Start.
func (c *CosmosChain) StartAllNodes(ctx context.Context) error {
	// prevent client calls during this time
	c.findTxMu.Lock()
	defer c.findTxMu.Unlock()
	var eg errgroup.Group
	for _, n := range c.Nodes() {
		n := n
		eg.Go(func() error {
			if err := n.CreateNodeContainer(ctx); err != nil {
				return err
			}
			return n.StartContainer(ctx)
		})
	}
	return eg.Wait()
}

// StartAllSidecars creates and starts new containers for each sidecar process.
// Should only be used if the chain has previously been started with .Start.
func (c *CosmosChain) StartAllSidecars(ctx context.Context) error {
	// prevent client calls during this time
	c.findTxMu.Lock()
	defer c.findTxMu.Unlock()
	var eg errgroup.Group
	for _, s := range c.Sidecars {
		s := s

		err := s.containerLifecycle.Running(ctx)
		if err == nil {
			continue
		}

		eg.Go(func() error {
			if err := s.CreateContainer(ctx); err != nil {
				return err
			}
			return s.StartContainer(ctx)
		})
	}
	return eg.Wait()
}

// StartAllValSidecars creates and starts new containers for each validator sidecar process.
// Should only be used if the chain has previously been started with .Start.
func (c *CosmosChain) StartAllValSidecars(ctx context.Context) error {
	// prevent client calls during this time
	c.findTxMu.Lock()
	defer c.findTxMu.Unlock()
	var eg errgroup.Group

	for _, v := range c.Validators {
		for _, s := range v.Sidecars {
			s := s

			err := s.containerLifecycle.Running(ctx)
			if err == nil {
				continue
			}

			eg.Go(func() error {
				if err := s.CreateContainer(ctx); err != nil {
					return err
				}
				return s.StartContainer(ctx)
			})
		}
	}

	return eg.Wait()
}

func (c *CosmosChain) VoteOnProposalAllValidators(ctx context.Context, proposalID int64, vote string) error {
	var eg errgroup.Group
	for _, n := range c.Nodes() {
		if n.Validator {
			n := n
			eg.Go(func() error {
				return n.VoteOnProposal(ctx, valKey, proposalID, vote)
			})
		}
	}
	return eg.Wait()
}
