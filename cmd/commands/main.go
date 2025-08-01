// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2024 The Lightning Network Developers

package commands

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/urfave/cli"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

const (
	defaultDataDir          = "data"
	defaultChainSubDir      = "chain"
	defaultTLSCertFilename  = "tls.cert"
	defaultMacaroonFilename = "admin.macaroon"
	defaultRPCPort          = "10009"
	defaultRPCHostPort      = "localhost:" + defaultRPCPort

	envVarRPCServer       = "LNCLI_RPCSERVER"
	envVarLNDDir          = "LNCLI_LNDDIR"
	envVarSOCKSProxy      = "LNCLI_SOCKSPROXY"
	envVarTLSCertPath     = "LNCLI_TLSCERTPATH"
	envVarChain           = "LNCLI_CHAIN"
	envVarNetwork         = "LNCLI_NETWORK"
	envVarMacaroonPath    = "LNCLI_MACAROONPATH"
	envVarMacaroonTimeout = "LNCLI_MACAROONTIMEOUT"
	envVarMacaroonIP      = "LNCLI_MACAROONIP"
	envVarProfile         = "LNCLI_PROFILE"
	envVarMacFromJar      = "LNCLI_MACFROMJAR"
)

var (
	DefaultLndDir      = btcutil.AppDataDir("lnd", false)
	defaultTLSCertPath = filepath.Join(
		DefaultLndDir, defaultTLSCertFilename,
	)

	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(lnrpc.MaxGrpcMsgSize)
)

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[lncli] %v\n", err)
	os.Exit(1)
}

func getWalletUnlockerClient(ctx *cli.Context) (lnrpc.WalletUnlockerClient,
	func()) {

	conn := getClientConn(ctx, true)

	cleanUp := func() {
		conn.Close()
	}

	return lnrpc.NewWalletUnlockerClient(conn), cleanUp
}

func getStateServiceClient(ctx *cli.Context) (lnrpc.StateClient, func()) {
	conn := getClientConn(ctx, true)

	cleanUp := func() {
		conn.Close()
	}

	return lnrpc.NewStateClient(conn), cleanUp
}

func getClient(ctx *cli.Context) (lnrpc.LightningClient, func()) {
	conn := getClientConn(ctx, false)

	cleanUp := func() {
		conn.Close()
	}

	return lnrpc.NewLightningClient(conn), cleanUp
}

func getClientConn(ctx *cli.Context, skipMacaroons bool) *grpc.ClientConn {
	// First, we'll get the selected stored profile or an ephemeral one
	// created from the global options in the CLI context.
	profile, err := getGlobalOptions(ctx, skipMacaroons)
	if err != nil {
		fatal(fmt.Errorf("could not load global options: %w", err))
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithUnaryInterceptor(
			addMetadataUnaryInterceptor(profile.Metadata),
		),
		grpc.WithStreamInterceptor(
			addMetaDataStreamInterceptor(profile.Metadata),
		),
	}

	if profile.Insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		// Load the specified TLS certificate.
		certPool, err := profile.cert()
		if err != nil {
			fatal(fmt.Errorf("could not create cert pool: %w", err))
		}

		// Build transport credentials from the certificate pool. If
		// there is no certificate pool, we expect the server to use a
		// non-self-signed certificate such as a certificate obtained
		// from Let's Encrypt.
		var creds credentials.TransportCredentials
		if certPool != nil {
			creds = credentials.NewClientTLSFromCert(certPool, "")
		} else {
			// Fallback to the system pool. Using an empty tls
			// config is an alternative to x509.SystemCertPool().
			// That call is not supported on Windows.
			creds = credentials.NewTLS(&tls.Config{})
		}

		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	// Only process macaroon credentials if --no-macaroons isn't set and
	// if we're not skipping macaroon processing.
	if !profile.NoMacaroons && !skipMacaroons {
		// Find out which macaroon to load.
		macName := profile.Macaroons.Default
		if ctx.GlobalIsSet("macfromjar") {
			macName = ctx.GlobalString("macfromjar")
		}
		var macEntry *macaroonEntry
		for _, entry := range profile.Macaroons.Jar {
			if entry.Name == macName {
				macEntry = entry
				break
			}
		}
		if macEntry == nil {
			fatal(fmt.Errorf("macaroon with name '%s' not found "+
				"in profile", macName))
		}

		// Get and possibly decrypt the specified macaroon.
		//
		// TODO(guggero): Make it possible to cache the password so we
		// don't need to ask for it every time.
		mac, err := macEntry.loadMacaroon(readPassword)
		if err != nil {
			fatal(fmt.Errorf("could not load macaroon: %w", err))
		}

		macConstraints := []macaroons.Constraint{
			// We add a time-based constraint to prevent replay of
			// the macaroon. It's good for 60 seconds by default to
			// make up for any discrepancy between client and server
			// clocks, but leaking the macaroon before it becomes
			// invalid makes it possible for an attacker to reuse
			// the macaroon. In addition, the validity time of the
			// macaroon is extended by the time the server clock is
			// behind the client clock, or shortened by the time the
			// server clock is ahead of the client clock (or invalid
			// altogether if, in the latter case, this time is more
			// than 60 seconds).
			// TODO(aakselrod): add better anti-replay protection.
			macaroons.TimeoutConstraint(profile.Macaroons.Timeout),

			// Lock macaroon down to a specific IP address.
			macaroons.IPLockConstraint(profile.Macaroons.IP),

			// ... Add more constraints if needed.
		}

		// Apply constraints to the macaroon.
		constrainedMac, err := macaroons.AddConstraints(
			mac, macConstraints...,
		)
		if err != nil {
			fatal(err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred, err := macaroons.NewMacaroonCredential(constrainedMac)
		if err != nil {
			fatal(fmt.Errorf("error cloning mac: %w", err))
		}
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	// If a socksproxy server is specified we use a tor dialer
	// to connect to the grpc server.
	if ctx.GlobalIsSet("socksproxy") {
		socksProxy := ctx.GlobalString("socksproxy")
		torDialer := func(_ context.Context, addr string) (net.Conn,
			error) {

			return tor.Dial(
				addr, socksProxy, false, false,
				tor.DefaultConnTimeout,
			)
		}
		opts = append(opts, grpc.WithContextDialer(torDialer))
	} else {
		// We need to use a custom dialer so we can also connect to
		// unix sockets and not just TCP addresses.
		genericDialer := lncfg.ClientAddressDialer(defaultRPCPort)
		opts = append(opts, grpc.WithContextDialer(genericDialer))
	}

	opts = append(opts, grpc.WithDefaultCallOptions(maxMsgRecvSize))

	conn, err := grpc.Dial(profile.RPCServer, opts...)
	if err != nil {
		fatal(fmt.Errorf("unable to connect to RPC server: %w", err))
	}

	return conn
}

// addMetadataUnaryInterceptor returns a grpc client side interceptor that
// appends any key-value metadata strings to the outgoing context of a grpc
// unary call.
func addMetadataUnaryInterceptor(
	md map[string]string) grpc.UnaryClientInterceptor {

	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption) error {

		outCtx := contextWithMetadata(ctx, md)
		return invoker(outCtx, method, req, reply, cc, opts...)
	}
}

// addMetaDataStreamInterceptor returns a grpc client side interceptor that
// appends any key-value metadata strings to the outgoing context of a grpc
// stream call.
func addMetaDataStreamInterceptor(
	md map[string]string) grpc.StreamClientInterceptor {

	return func(ctx context.Context, desc *grpc.StreamDesc,
		cc *grpc.ClientConn, method string, streamer grpc.Streamer,
		opts ...grpc.CallOption) (grpc.ClientStream, error) {

		outCtx := contextWithMetadata(ctx, md)
		return streamer(outCtx, desc, cc, method, opts...)
	}
}

// contextWithMetaData appends the given metadata key-value pairs to the given
// context.
func contextWithMetadata(ctx context.Context,
	md map[string]string) context.Context {

	kvPairs := make([]string, 0, 2*len(md))
	for k, v := range md {
		kvPairs = append(kvPairs, k, v)
	}

	return metadata.AppendToOutgoingContext(ctx, kvPairs...)
}

// extractPathArgs parses the TLS certificate and macaroon paths from the
// command.
func extractPathArgs(ctx *cli.Context) (string, string, error) {
	network := strings.ToLower(ctx.GlobalString("network"))
	switch network {
	case "mainnet", "testnet", "testnet4", "regtest", "simnet", "signet":
	default:
		return "", "", fmt.Errorf("unknown network: %v", network)
	}

	// We'll now fetch the lnddir so we can make a decision  on how to
	// properly read the macaroons (if needed) and also the cert. This will
	// either be the default, or will have been overwritten by the end
	// user.
	lndDir := lncfg.CleanAndExpandPath(ctx.GlobalString("lnddir"))

	// If the macaroon path as been manually provided, then we'll only
	// target the specified file.
	var macPath string
	if ctx.GlobalString("macaroonpath") != "" {
		macPath = lncfg.CleanAndExpandPath(ctx.GlobalString(
			"macaroonpath",
		))
	} else {
		// Otherwise, we'll go into the path:
		// lnddir/data/chain/<chain>/<network> in order to fetch the
		// macaroon that we need.
		macPath = filepath.Join(
			lndDir, defaultDataDir, defaultChainSubDir,
			lnd.BitcoinChainName, network, defaultMacaroonFilename,
		)
	}

	tlsCertPath := lncfg.CleanAndExpandPath(ctx.GlobalString("tlscertpath"))

	// If a custom lnd directory was set, we'll also check if custom paths
	// for the TLS cert and macaroon file were set as well. If not, we'll
	// override their paths so they can be found within the custom lnd
	// directory set. This allows us to set a custom lnd directory, along
	// with custom paths to the TLS cert and macaroon file.
	if lndDir != DefaultLndDir {
		tlsCertPath = filepath.Join(lndDir, defaultTLSCertFilename)
	}

	return tlsCertPath, macPath, nil
}

// checkNotBothSet accepts two flag names, a and b, and checks that only flag a
// or flag b can be set, but not both. It returns the name of the flag or an
// error.
func checkNotBothSet(ctx *cli.Context, a, b string) (string, error) {
	if ctx.IsSet(a) && ctx.IsSet(b) {
		return "", fmt.Errorf(
			"either %s or %s should be set, but not both", a, b,
		)
	}

	if ctx.IsSet(a) {
		return a, nil
	}

	return b, nil
}

func Main() {
	app := cli.NewApp()
	app.Name = "lncli"
	app.Version = build.Version() + " commit=" + build.Commit
	app.Usage = "control plane for your Lightning Network Daemon (lnd)"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "rpcserver",
			Value:  defaultRPCHostPort,
			Usage:  "The host:port of LN daemon.",
			EnvVar: envVarRPCServer,
		},
		cli.StringFlag{
			Name:      "lnddir",
			Value:     DefaultLndDir,
			Usage:     "The path to lnd's base directory.",
			TakesFile: true,
			EnvVar:    envVarLNDDir,
		},
		cli.StringFlag{
			Name: "socksproxy",
			Usage: "The host:port of a SOCKS proxy through " +
				"which all connections to the LN " +
				"daemon will be established over.",
			EnvVar: envVarSOCKSProxy,
		},
		cli.StringFlag{
			Name:      "tlscertpath",
			Value:     defaultTLSCertPath,
			Usage:     "The path to lnd's TLS certificate.",
			TakesFile: true,
			EnvVar:    envVarTLSCertPath,
		},
		cli.StringFlag{
			Name:   "chain, c",
			Usage:  "The chain lnd is running on, e.g. bitcoin.",
			Value:  "bitcoin",
			EnvVar: envVarChain,
		},
		cli.StringFlag{
			Name: "network, n",
			Usage: "The network lnd is running on; valid values " +
				"are: mainnet, testnet, testnet4, regtest, " +
				"signet and simnet.",
			Value:  "mainnet",
			EnvVar: envVarNetwork,
		},
		cli.BoolFlag{
			Name:  "no-macaroons",
			Usage: "Disable macaroon authentication.",
		},
		cli.StringFlag{
			Name:      "macaroonpath",
			Usage:     "The path to macaroon file.",
			TakesFile: true,
			EnvVar:    envVarMacaroonPath,
		},
		cli.Int64Flag{
			Name:  "macaroontimeout",
			Value: 60,
			Usage: "Anti-replay macaroon validity time in " +
				"seconds.",
			EnvVar: envVarMacaroonTimeout,
		},
		cli.StringFlag{
			Name:   "macaroonip",
			Usage:  "If set, lock macaroon to specific IP address.",
			EnvVar: envVarMacaroonIP,
		},
		cli.StringFlag{
			Name: "profile, p",
			Usage: "Instead of reading settings from command " +
				"line parameters or using the default " +
				"profile, use a specific profile. If " +
				"a default profile is set, this flag can be " +
				"set to an empty string to disable reading " +
				"values from the profiles file.",
			EnvVar: envVarProfile,
		},
		cli.StringFlag{
			Name: "macfromjar",
			Usage: "Use this macaroon from the profile's " +
				"macaroon jar instead of the default one. " +
				"Can only be used if profiles are defined.",
			EnvVar: envVarMacFromJar,
		},
		cli.StringSliceFlag{
			Name: "metadata",
			Usage: "This flag can be used to specify a key-value " +
				"pair that should be appended to the " +
				"outgoing context before the request is sent " +
				"to lnd. This flag may be specified multiple " +
				"times. The format is: \"key:value\".",
		},
		cli.BoolFlag{
			Name: "insecure",
			Usage: "Connect to the rpc server without TLS " +
				"authentication",
			Hidden: true,
		},
	}
	app.Commands = []cli.Command{
		createCommand,
		createWatchOnlyCommand,
		unlockCommand,
		changePasswordCommand,
		newAddressCommand,
		estimateFeeCommand,
		sendManyCommand,
		sendCoinsCommand,
		listUnspentCommand,
		connectCommand,
		disconnectCommand,
		openChannelCommand,
		batchOpenChannelCommand,
		closeChannelCommand,
		closeAllChannelsCommand,
		abandonChannelCommand,
		listPeersCommand,
		walletBalanceCommand,
		ChannelBalanceCommand,
		getInfoCommand,
		getDebugInfoCommand,
		encryptDebugPackageCommand,
		decryptDebugPackageCommand,
		getRecoveryInfoCommand,
		pendingChannelsCommand,
		SendPaymentCommand,
		payInvoiceCommand,
		sendToRouteCommand,
		AddInvoiceCommand,
		lookupInvoiceCommand,
		listInvoicesCommand,
		deleteCanceledInvoiceCommand,
		ListChannelsCommand,
		closedChannelsCommand,
		listPaymentsCommand,
		describeGraphCommand,
		getNodeMetricsCommand,
		getChanInfoCommand,
		getNodeInfoCommand,
		queryRoutesCommand,
		getNetworkInfoCommand,
		debugLevelCommand,
		decodePayReqCommand,
		listChainTxnsCommand,
		stopCommand,
		signMessageCommand,
		verifyMessageCommand,
		feeReportCommand,
		updateChannelPolicyCommand,
		forwardingHistoryCommand,
		exportChanBackupCommand,
		verifyChanBackupCommand,
		restoreChanBackupCommand,
		bakeMacaroonCommand,
		listMacaroonIDsCommand,
		deleteMacaroonIDCommand,
		listPermissionsCommand,
		printMacaroonCommand,
		constrainMacaroonCommand,
		trackPaymentCommand,
		versionCommand,
		profileSubCommand,
		getStateCommand,
		deletePaymentsCommand,
		sendCustomCommand,
		subscribeCustomCommand,
		fishCompletionCommand,
		listAliasesCommand,
		estimateRouteFeeCommand,
		generateManPageCommand,
	}

	// Add any extra commands determined by build flags.
	app.Commands = append(app.Commands, autopilotCommands()...)
	app.Commands = append(app.Commands, invoicesCommands()...)
	app.Commands = append(app.Commands, neutrinoCommands()...)
	app.Commands = append(app.Commands, routerCommands()...)
	app.Commands = append(app.Commands, walletCommands()...)
	app.Commands = append(app.Commands, watchtowerCommands()...)
	app.Commands = append(app.Commands, wtclientCommands()...)
	app.Commands = append(app.Commands, devCommands()...)
	app.Commands = append(app.Commands, peersCommands()...)
	app.Commands = append(app.Commands, chainCommands()...)

	if err := app.Run(os.Args); err != nil {
		fatal(err)
	}
}

// readPassword reads a password from the terminal. This requires there to be an
// actual TTY so passing in a password from stdin won't work.
func readPassword(text string) ([]byte, error) {
	fmt.Print(text)

	// The variable syscall.Stdin is of a different type in the Windows API
	// that's why we need the explicit cast. And of course the linter
	// doesn't like it either.
	pw, err := term.ReadPassword(int(syscall.Stdin)) //nolint:unconvert
	fmt.Println()

	return pw, err
}

// networkParams parses the global network flag into a chaincfg.Params.
func networkParams(ctx *cli.Context) (*chaincfg.Params, error) {
	network := strings.ToLower(ctx.GlobalString("network"))
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, nil

	case "testnet", "testnet3":
		return &chaincfg.TestNet3Params, nil

	case "testnet4":
		return &chaincfg.TestNet4Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	case "signet":
		return &chaincfg.SigNetParams, nil

	default:
		return nil, fmt.Errorf("unknown network: %v", network)
	}
}

// parseCoinSelectionStrategy parses a coin selection strategy string
// from the CLI to its lnrpc.CoinSelectionStrategy counterpart proto type.
func parseCoinSelectionStrategy(ctx *cli.Context) (
	lnrpc.CoinSelectionStrategy, error) {

	strategy := ctx.String(coinSelectionStrategyFlag.Name)
	if !ctx.IsSet(coinSelectionStrategyFlag.Name) {
		return lnrpc.CoinSelectionStrategy_STRATEGY_USE_GLOBAL_CONFIG,
			nil
	}

	switch strategy {
	case "global-config":
		return lnrpc.CoinSelectionStrategy_STRATEGY_USE_GLOBAL_CONFIG,
			nil

	case "largest":
		return lnrpc.CoinSelectionStrategy_STRATEGY_LARGEST, nil

	case "random":
		return lnrpc.CoinSelectionStrategy_STRATEGY_RANDOM, nil

	default:
		return 0, fmt.Errorf("unknown coin selection strategy "+
			"%v", strategy)
	}
}
