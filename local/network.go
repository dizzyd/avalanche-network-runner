package local

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ava-labs/avalanche-network-runner/api"
	"github.com/ava-labs/avalanche-network-runner/network"
	"github.com/ava-labs/avalanche-network-runner/network/node"
	"github.com/ava-labs/avalanche-network-runner/utils"
	"github.com/ava-labs/avalanchego/config"
	avalancheconstants "github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"golang.org/x/sync/errgroup"
)

const (
	defaultNodeNamePrefix = "node-"
	configFileName        = "config.json"
	stakingKeyFileName    = "staking.key"
	stakingCertFileName   = "staking.crt"
	genesisFileName       = "genesis.json"
	stopTimeout           = 30 * time.Second
	healthCheckFreq       = 3 * time.Second
	defaultNumNodes       = 5
)

// interface compliance
var (
	_ network.Network    = (*localNetwork)(nil)
	_ NodeProcessCreator = (*nodeProcessCreator)(nil)

	warnFlags = map[string]struct{}{
		config.NetworkNameKey:  {},
		config.BootstrapIPsKey: {},
		config.BootstrapIDsKey: {},
	}
)

// network keeps information uses for network management, and accessing all the nodes
type localNetwork struct {
	lock sync.RWMutex
	log  logging.Logger
	// This network's ID.
	networkID uint32
	// This network's genesis file.
	// Must not be nil.
	genesis []byte
	// Used to create a new API client
	newAPIClientF api.NewAPIClientF
	// Used to create new node processes
	nodeProcessCreator NodeProcessCreator
	// Closed when network is done shutting down
	closedOnStopCh chan struct{}
	// For node name generation
	nextNodeSuffix uint64
	// Node Name --> Node
	nodes map[string]*localNode
	// List of nodes that new nodes will bootstrap from.
	bootstrapIPs, bootstrapIDs beaconList
	// rootDir is the root directory under which we write all node
	// logs, databases, etc.
	rootDir string
	// Flags to apply to all nodes if not present
	flags map[string]interface{}
}

var (
	//go:embed default
	embeddedDefaultNetworkConfigDir embed.FS
	// Pre-defined network configuration. The ImplSpecificConfig
	// field of each node in [defaultNetworkConfig.NodeConfigs]
	// is not defined.
	// [defaultNetworkConfig] should not be modified.
	// TODO add method Copy() to network.Config to prevent
	// accidental overwriting
	defaultNetworkConfig network.Config
)

// populate default network config from embedded default directory
func init() {
	configsDir, err := fs.Sub(embeddedDefaultNetworkConfigDir, "default")
	if err != nil {
		panic(err)
	}

	defaultNetworkConfig = network.Config{
		Name:        "my network",
		NodeConfigs: make([]node.Config, defaultNumNodes),
		LogLevel:    "INFO",
	}

	genesis, err := fs.ReadFile(configsDir, "genesis.json")
	if err != nil {
		panic(err)
	}
	defaultNetworkConfig.Genesis = string(genesis)

	for i := 0; i < len(defaultNetworkConfig.NodeConfigs); i++ {
		configFile, err := fs.ReadFile(configsDir, fmt.Sprintf("node%d/config.json", i))
		if err != nil {
			panic(err)
		}
		defaultNetworkConfig.NodeConfigs[i].ConfigFile = string(configFile)
		stakingKey, err := fs.ReadFile(configsDir, fmt.Sprintf("node%d/staking.key", i))
		if err != nil {
			panic(err)
		}
		defaultNetworkConfig.NodeConfigs[i].StakingKey = string(stakingKey)
		stakingCert, err := fs.ReadFile(configsDir, fmt.Sprintf("node%d/staking.crt", i))
		if err != nil {
			panic(err)
		}
		cChainConfig, err := fs.ReadFile(configsDir, fmt.Sprintf("node%d/cchain_config.json", i))
		if err != nil {
			panic(err)
		}
		defaultNetworkConfig.NodeConfigs[i].CChainConfigFile = string(cChainConfig)
		defaultNetworkConfig.NodeConfigs[i].StakingCert = string(stakingCert)
		defaultNetworkConfig.NodeConfigs[i].IsBeacon = true
	}
}

// NodeProcessCreator is an interface for new node process creation
type NodeProcessCreator interface {
	NewNodeProcess(config node.Config, args ...string) (NodeProcess, error)
}

type nodeProcessCreator struct {
	// If this node's stdout or stderr are redirected, [colorPicker] determines
	// the color of logs printed to stdout and/or stderr
	colorPicker utils.ColorPicker
	// If this node's stdout is redirected, it will be to here.
	// In practice this is usually os.Stdout, but for testing can be replaced.
	stdout io.Writer
	// If this node's stderr is redirected, it will be to here.
	// In practice this is usually os.Stderr, but for testing can be replaced.
	stderr io.Writer
}

// NewNodeProcess creates a new process of the passed binary
// If the config has redirection set to `true` for either StdErr or StdOut,
// the output will be redirected and colored
func (npc *nodeProcessCreator) NewNodeProcess(config node.Config, args ...string) (NodeProcess, error) {
	var localNodeConfig NodeConfig
	if err := json.Unmarshal(config.ImplSpecificConfig, &localNodeConfig); err != nil {
		return nil, fmt.Errorf("couldn't unmarshal local.NodeConfig: %w", err)
	}
	// Start the AvalancheGo node and pass it the flags defined above
	cmd := exec.Command(localNodeConfig.BinaryPath, args...)
	// assign a new color to this process (might not be used if the localNodeConfig isn't set for it)
	color := npc.colorPicker.NextColor()
	// Optionally redirect stdout and stderr
	if localNodeConfig.RedirectStdout {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("Could not create stdout pipe: %s", err)
		}
		// redirect stdout and assign a color to the text
		utils.ColorAndPrepend(stdout, npc.stdout, config.Name, color)
	}
	if localNodeConfig.RedirectStderr {
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("Could not create stderr pipe: %s", err)
		}
		// redirect stderr and assign a color to the text
		utils.ColorAndPrepend(stderr, npc.stderr, config.Name, color)
	}
	return &nodeProcessImpl{cmd: cmd}, nil
}

type beaconList map[string]struct{}

func (l beaconList) String() string {
	if len(l) == 0 {
		return ""
	}
	s := strings.Builder{}
	i := 0
	for beacon := range l {
		if i != 0 {
			_, _ = s.WriteString(",")
		}
		_, _ = s.WriteString(beacon)
		i++
	}
	return s.String()
}

// NewNetwork call newNetwork with no mocking
func NewNetwork(
	log logging.Logger,
	networkConfig network.Config,
) (network.Network, error) {
	return NewNetworkWithDir(log, networkConfig, "")
}

func NewNetworkWithDir(log logging.Logger, networkConfig network.Config, networkDir string) (network.Network, error) {
	return newNetwork(log, networkConfig, api.NewAPIClient, &nodeProcessCreator{
		colorPicker: utils.NewColorPicker(),
		stdout:      os.Stdout,
		stderr:      os.Stderr,
	}, networkDir)
}

// newNetwork creates a network from given configuration
func newNetwork(
	log logging.Logger,
	networkConfig network.Config,
	newAPIClientF api.NewAPIClientF,
	nodeProcessCreator NodeProcessCreator,
	networkDir string,
) (network.Network, error) {
	if err := networkConfig.Validate(); err != nil {
		return nil, fmt.Errorf("config failed validation: %w", err)
	}
	log.Info("creating network with %d nodes", len(networkConfig.NodeConfigs))

	networkID, err := utils.NetworkIDFromGenesis([]byte(networkConfig.Genesis))
	if err != nil {
		return nil, fmt.Errorf("couldn't get network ID from genesis: %w", err)
	}

	// Create the network
	net := &localNetwork{
		networkID:          networkID,
		genesis:            []byte(networkConfig.Genesis),
		nodes:              map[string]*localNode{},
		closedOnStopCh:     make(chan struct{}),
		log:                log,
		bootstrapIPs:       make(beaconList),
		bootstrapIDs:       make(beaconList),
		newAPIClientF:      newAPIClientF,
		nodeProcessCreator: nodeProcessCreator,
		flags:              networkConfig.Flags,
	}

	// Sort node configs so beacons start first
	var nodeConfigs []node.Config
	for _, nodeConfig := range networkConfig.NodeConfigs {
		if nodeConfig.IsBeacon {
			nodeConfigs = append(nodeConfigs, nodeConfig)
		}
	}
	for _, nodeConfig := range networkConfig.NodeConfigs {
		if !nodeConfig.IsBeacon {
			nodeConfigs = append(nodeConfigs, nodeConfig)
		}
	}

	if networkDir == "" {
		net.rootDir, err = os.MkdirTemp("", "avalanche-network-runner-*")
		if err != nil {
			return nil, err
		}
	} else {
		net.rootDir = networkDir
	}

	for _, nodeConfig := range nodeConfigs {
		if _, err := net.addNode(nodeConfig); err != nil {
			if err := net.stop(context.Background()); err != nil {
				// Clean up nodes already created
				log.Debug("error stopping network: %s", err)
			}
			return nil, fmt.Errorf("error adding node %s: %s", nodeConfig.Name, err)
		}
	}
	return net, nil
}

// NewDefaultNetwork returns a new network using a pre-defined
// network configuration.
// The following addresses are pre-funded:
// X-Chain Address 1:     X-custom18jma8ppw3nhx5r4ap8clazz0dps7rv5u9xde7p
// X-Chain Address 1 Key: PrivateKey-ewoqjP7PxY4yr3iLTpLisriqt94hdyDFNgchSxGGztUrTXtNN
// X-Chain Address 2:     X-custom16045mxr3s2cjycqe2xfluk304xv3ezhkhsvkpr
// X-Chain Address 2 Key: PrivateKey-2fzYBh3bbWemKxQmMfX6DSuL2BFmDSLQWTvma57xwjQjtf8gFq
// P-Chain Address 1:     P-custom18jma8ppw3nhx5r4ap8clazz0dps7rv5u9xde7p
// P-Chain Address 1 Key: PrivateKey-ewoqjP7PxY4yr3iLTpLisriqt94hdyDFNgchSxGGztUrTXtNN
// P-Chain Address 2:     P-custom16045mxr3s2cjycqe2xfluk304xv3ezhkhsvkpr
// P-Chain Address 2 Key: PrivateKey-2fzYBh3bbWemKxQmMfX6DSuL2BFmDSLQWTvma57xwjQjtf8gFq
// C-Chain Address:       0x8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC
// C-Chain Address Key:   56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027
// The following nodes are validators:
// * NodeID-7Xhw2mDxuDS44j42TCB6U5579esbSt3Lg
// * NodeID-MFrZFVCXPv5iCn6M9K6XduxGTYp891xXZ
// * NodeID-NFBbbJ4qCmNaCzeW7sxErhvWqvEQMnYcN
// * NodeID-GWPcbFJZFfZreETSoWjPimr846mXEKCtu
// * NodeID-P7oB2McjBGgW2NXXWVYjV8JEDFoW9xDE5
func NewDefaultNetwork(
	log logging.Logger,
	binaryPath string,
) (network.Network, error) {
	return newDefaultNetwork(log, binaryPath, api.NewAPIClient, &nodeProcessCreator{
		colorPicker: utils.NewColorPicker(),
		stdout:      os.Stdout,
		stderr:      os.Stderr,
	})
}

func newDefaultNetwork(
	log logging.Logger,
	binaryPath string,
	newAPIClientF api.NewAPIClientF,
	nodeProcessCreator NodeProcessCreator,
) (network.Network, error) {
	config := NewDefaultConfig(binaryPath)
	return newNetwork(log, config, newAPIClientF, nodeProcessCreator, "")
}

// NewDefaultConfig creates a new default network config
func NewDefaultConfig(binaryPath string) network.Config {
	config := defaultNetworkConfig
	// Don't overwrite [DefaultNetworkConfig.NodeConfigs]
	config.NodeConfigs = make([]node.Config, len(defaultNetworkConfig.NodeConfigs))
	copy(config.NodeConfigs, defaultNetworkConfig.NodeConfigs)
	for i := 0; i < len(config.NodeConfigs); i++ {
		config.NodeConfigs[i].ImplSpecificConfig = utils.NewLocalNodeConfigJsonRaw(binaryPath)
	}
	return config
}

// See network.Network
func (ln *localNetwork) AddNode(nodeConfig node.Config) (node.Node, error) {
	ln.lock.Lock()
	defer ln.lock.Unlock()

	return ln.addNode(nodeConfig)
}

// Assumes [ln.lock] is held.
// TODO make this method shorter
func (ln *localNetwork) addNode(nodeConfig node.Config) (node.Node, error) {
	if ln.isStopped() {
		return nil, network.ErrStopped
	}

	for flagName, flagVal := range ln.flags {
		if nodeConfig.Flags == nil {
			nodeConfig.Flags = make(map[string]interface{})
		}
		// If the same flag is given in network config and node config,
		// the flag in the node config takes precedence
		if val, ok := nodeConfig.Flags[flagName]; !ok {
			nodeConfig.Flags[flagName] = flagVal
		} else {
			ln.log.Info(
				"not overwriting node config flag %s (value %v) with network config flag (value %v)",
				flagName, val, flagVal,
			)
		}
	}

	// If no name was given, use default name pattern
	if len(nodeConfig.Name) == 0 {
		nodeConfig.Name = fmt.Sprintf("%s%d", defaultNodeNamePrefix, ln.nextNodeSuffix)
		ln.nextNodeSuffix++
	}

	// Enforce name uniqueness
	if _, ok := ln.nodes[nodeConfig.Name]; ok {
		return nil, fmt.Errorf("repeated node name %s", nodeConfig.Name)
	}

	if ln.rootDir == "" {
		ln.log.Warn("no network root directory defined; will create this node's runtime directory in working directory")
	}
	// [nodeRootDir] is where this node's config file, C-Chain config file,
	// staking key, staking certificate and genesis file will be written.
	// (Other file locations are given in the node's config file.)
	// TODO should we do this for other directories? Profiles?
	nodeRootDir := filepath.Join(ln.rootDir, nodeConfig.Name)
	if err := os.Mkdir(nodeRootDir, 0o755); err != nil {
		if os.IsExist(err) {
			ln.log.Warn("node root directory %s already exists", nodeRootDir)
		} else {
			return nil, fmt.Errorf("error creating temp dir: %w", err)
		}
	}

	// If config file is given, don't overwrite API port, P2P port, DB path, logs path
	var configFile map[string]interface{}
	if len(nodeConfig.ConfigFile) != 0 {
		if err := json.Unmarshal([]byte(nodeConfig.ConfigFile), &configFile); err != nil {
			return nil, fmt.Errorf("couldn't unmarshal config file: %w", err)
		}
	}

	// Tell the node to put the database in [tmpDir], unless given in config file
	dbPath := nodeRootDir
	if dbPathIntf, ok := configFile[config.DBPathKey]; ok {
		if dbPathFromConfig, ok := dbPathIntf.(string); ok {
			dbPath = dbPathFromConfig
		} else {
			return nil, fmt.Errorf("expected flag %q to be string but got %T", config.DBPathKey, dbPathIntf)
		}
	}

	// Tell the node to put the log directory in [tmpDir/logs], unless given in config file
	logsDir := filepath.Join(nodeRootDir, "logs")
	if logsDirIntf, ok := configFile[config.LogsDirKey]; ok {
		if logsDirFromConfig, ok := logsDirIntf.(string); ok {
			logsDir = logsDirFromConfig
		} else {
			return nil, fmt.Errorf("expected flag %q to be string but got %T", config.LogsDirKey, logsDirIntf)
		}
	}

	// Use random free API port, unless given in node config flags or config file
	var (
		apiPort uint16
		err     error
	)
	if apiPortIntf, ok := nodeConfig.Flags[config.HTTPPortKey]; ok {
		if apiPortFromNodeConfigFlags, ok := apiPortIntf.(int); ok {
			apiPort = uint16(apiPortFromNodeConfigFlags)
		} else {
			return nil, fmt.Errorf("expected flag %q to be int but got %T", config.HTTPPortKey, apiPortIntf)
		}
	} else if apiPortIntf, ok := configFile[config.HTTPPortKey]; ok {
		if apiPortFromConfigFile, ok := apiPortIntf.(float64); ok {
			apiPort = uint16(apiPortFromConfigFile)
		} else {
			return nil, fmt.Errorf("expected flag %q to be float64 but got %T", config.HTTPPortKey, apiPortIntf)
		}
	} else {
		// Use a random free port.
		// Note: it is possible but unlikely for getFreePort to return the same port multiple times.
		apiPort, err = getFreePort()
		if err != nil {
			return nil, fmt.Errorf("couldn't get free API port: %w", err)
		}
	}

	// Use a random free P2P (staking) port, unless given in node config flags or config file
	var p2pPort uint16
	if p2pPortIntf, ok := nodeConfig.Flags[config.StakingPortKey]; ok {
		if p2pPortFromNodeConfigFlags, ok := p2pPortIntf.(int); ok {
			p2pPort = uint16(p2pPortFromNodeConfigFlags)
		} else {
			return nil, fmt.Errorf("expected flag %q to be int but got %T", config.StakingPortKey, p2pPortIntf)
		}
	} else if p2pPortIntf, ok := configFile[config.StakingPortKey]; ok {
		if p2pPortFromConfigFile, ok := p2pPortIntf.(float64); ok {
			p2pPort = uint16(p2pPortFromConfigFile)
		} else {
			return nil, fmt.Errorf("expected flag %q to be float64 but got %T", config.StakingPortKey, p2pPortIntf)
		}
	} else {
		// Use a random free port.
		// Note: it is possible but unlikely for getFreePort to return the same port multiple times.
		p2pPort, err = getFreePort()
		if err != nil {
			return nil, fmt.Errorf("couldn't get free P2P port: %w", err)
		}
	}

	// Flags for AvalancheGo
	flags := []string{
		fmt.Sprintf("--%s=%d", config.NetworkNameKey, ln.networkID),
		fmt.Sprintf("--%s=%s", config.DBPathKey, dbPath),
		fmt.Sprintf("--%s=%s", config.LogsDirKey, logsDir),
		fmt.Sprintf("--%s=%d", config.HTTPPortKey, apiPort),
		fmt.Sprintf("--%s=%d", config.StakingPortKey, p2pPort),
		fmt.Sprintf("--%s=%s", config.BootstrapIPsKey, ln.bootstrapIPs),
		fmt.Sprintf("--%s=%s", config.BootstrapIDsKey, ln.bootstrapIDs),
	}

	for flagName, flagVal := range nodeConfig.Flags {
		if _, ok := warnFlags[flagName]; ok {
			ln.log.Warn("The flag %s has been provided. This can create conflicts with the runner. The suggestion is to remove this flag", flagName)
		}
		flags = append(flags, fmt.Sprintf("--%s=%v", flagName, flagVal))
	}

	// Parse this node's ID
	nodeID, err := utils.ToNodeID([]byte(nodeConfig.StakingKey), []byte(nodeConfig.StakingCert))
	if err != nil {
		return nil, fmt.Errorf("couldn't create node ID: %w", err)
	}

	// If this node is a beacon, add its IP/ID to the beacon lists.
	// Note that we do this *after* we set this node's bootstrap IPs/IDs
	// so this node won't try to use itself as a beacon.
	if nodeConfig.IsBeacon {
		ln.bootstrapIDs[nodeID.PrefixedString(avalancheconstants.NodeIDPrefix)] = struct{}{}
		ln.bootstrapIPs[fmt.Sprintf("127.0.0.1:%d", p2pPort)] = struct{}{}
	}

	ln.log.Info(
		"adding node %q with tmp dir at %s, logs at %s, DB at %s, P2P port %d, API port %d",
		nodeConfig.Name, nodeRootDir, logsDir, dbPath, p2pPort, apiPort,
	)

	// Write this node's staking key/cert to disk.
	stakingKeyFilePath := filepath.Join(nodeRootDir, stakingKeyFileName)
	if err := createFileAndWrite(stakingKeyFilePath, []byte(nodeConfig.StakingKey)); err != nil {
		return nil, fmt.Errorf("error creating/writing staking key: %w", err)
	}
	flags = append(flags, fmt.Sprintf("--%s=%s", config.StakingKeyPathKey, stakingKeyFilePath))
	stakingCertFilePath := filepath.Join(nodeRootDir, stakingCertFileName)
	if err := createFileAndWrite(stakingCertFilePath, []byte(nodeConfig.StakingCert)); err != nil {
		return nil, fmt.Errorf("error creating/writing staking cert: %w", err)
	}
	flags = append(flags, fmt.Sprintf("--%s=%s", config.StakingCertPathKey, stakingCertFilePath))

	// Write this node's config file to disk if one is given.
	configFilePath := filepath.Join(nodeRootDir, configFileName)
	if len(nodeConfig.ConfigFile) != 0 {
		if err := createFileAndWrite(configFilePath, []byte(nodeConfig.ConfigFile)); err != nil {
			return nil, fmt.Errorf("error creating/writing config file: %w", err)
		}
		flags = append(flags, fmt.Sprintf("--%s=%s", config.ConfigFileKey, configFilePath))
	}

	// Write this node's genesis file to disk.
	genesisFilePath := filepath.Join(nodeRootDir, genesisFileName)
	if err := createFileAndWrite(genesisFilePath, ln.genesis); err != nil {
		return nil, fmt.Errorf("error creating/writing genesis file: %w", err)
	}
	flags = append(flags, fmt.Sprintf("--%s=%s", config.GenesisConfigFileKey, genesisFilePath))

	// Write this node's C-Chain file to disk if one is given.
	if len(nodeConfig.CChainConfigFile) != 0 {
		cChainConfigFilePath := filepath.Join(nodeRootDir, "C", configFileName)
		if err := createFileAndWrite(cChainConfigFilePath, []byte(nodeConfig.CChainConfigFile)); err != nil {
			return nil, fmt.Errorf("error creating/writing C-Chain config file: %w", err)
		}
		flags = append(flags, fmt.Sprintf("--%s=%s", config.ChainConfigDirKey, nodeRootDir))
	}

	var localNodeConfig NodeConfig
	if err := json.Unmarshal(nodeConfig.ImplSpecificConfig, &localNodeConfig); err != nil {
		return nil, fmt.Errorf("Unmarshalling an expected local.NodeConfig object failed: %w", err)
	}

	// Start the AvalancheGo node and pass it the flags defined above
	nodeProcess, err := ln.nodeProcessCreator.NewNodeProcess(nodeConfig, flags...)
	if err != nil {
		return nil, fmt.Errorf("couldn't create new node process: %s", err)
	}
	ln.log.Debug("starting node %q with \"%s %s\"", nodeConfig.Name, localNodeConfig.BinaryPath, flags)
	if err := nodeProcess.Start(); err != nil {
		return nil, fmt.Errorf("could not execute cmd \"%s %s\": %w", localNodeConfig.BinaryPath, flags, err)
	}

	// Create a wrapper for this node so we can reference it later
	node := &localNode{
		name:    nodeConfig.Name,
		nodeID:  nodeID,
		client:  ln.newAPIClientF("localhost", apiPort),
		process: nodeProcess,
		apiPort: apiPort,
		p2pPort: p2pPort,
	}
	ln.nodes[node.name] = node
	return node, nil
}

// See network.Network
func (ln *localNetwork) Healthy(ctx context.Context) chan error {
	ln.lock.RLock()
	defer ln.lock.RUnlock()

	healthyChan := make(chan error, 1)

	// Return unhealthy if the network is stopped
	if ln.isStopped() {
		healthyChan <- network.ErrStopped
		return healthyChan
	}

	nodes := make([]*localNode, 0, len(ln.nodes))
	for _, node := range ln.nodes {
		nodes = append(nodes, node)
	}
	go func() {
		errGr, ctx := errgroup.WithContext(ctx)
		for _, node := range nodes {
			node := node
			errGr.Go(func() error {
				// Every constants.HealthCheckInterval, query node for health status.
				// Do this until ctx timeout
				for {
					select {
					case <-ln.closedOnStopCh:
						return network.ErrStopped
					case <-ctx.Done():
						return fmt.Errorf("node %q failed to become healthy within timeout", node.GetName())
					case <-time.After(healthCheckFreq):
					}
					health, err := node.client.HealthAPI().Health(ctx)
					if err == nil && health.Healthy {
						ln.log.Debug("node %q became healthy", node.name)
						return nil
					}
				}
			})
		}
		// Wait until all nodes are ready or timeout
		if err := errGr.Wait(); err != nil {
			healthyChan <- err
		}
		close(healthyChan)
	}()
	return healthyChan
}

// See network.Network
func (ln *localNetwork) GetNode(nodeName string) (node.Node, error) {
	ln.lock.RLock()
	defer ln.lock.RUnlock()

	if ln.isStopped() {
		return nil, network.ErrStopped
	}

	node, ok := ln.nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("node %q not found in network", nodeName)
	}
	return node, nil
}

// See network.Network
func (ln *localNetwork) GetNodeNames() ([]string, error) {
	ln.lock.RLock()
	defer ln.lock.RUnlock()

	if ln.isStopped() {
		return nil, network.ErrStopped
	}

	names := make([]string, len(ln.nodes))
	i := 0
	for name := range ln.nodes {
		names[i] = name
		i++
	}
	return names, nil
}

// See network.Network
func (ln *localNetwork) GetAllNodes() (map[string]node.Node, error) {
	ln.lock.RLock()
	defer ln.lock.RUnlock()

	if ln.isStopped() {
		return nil, network.ErrStopped
	}

	nodesCopy := make(map[string]node.Node, len(ln.nodes))
	for name, node := range ln.nodes {
		nodesCopy[name] = node
	}
	return nodesCopy, nil
}

func (ln *localNetwork) Stop(ctx context.Context) error {
	ln.lock.Lock()
	defer ln.lock.Unlock()

	return ln.stop(ctx)
}

// Assumes [net.lock] is held
func (ln *localNetwork) stop(ctx context.Context) error {
	if ln.isStopped() {
		ln.log.Debug("stop() called multiple times")
		return network.ErrStopped
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	errs := wrappers.Errs{}
	for nodeName := range ln.nodes {
		select {
		case <-ctx.Done():
			// In practice we'll probably never time out here,
			// and the caller probably won't cancel a call
			// to stop(), but we include this to respect the
			// network.Network interface.
			return ctx.Err()
		default:
		}
		if err := ln.removeNode(nodeName); err != nil {
			ln.log.Error("error stopping node %q: %s", nodeName, err)
			errs.Add(err)
		}
	}
	close(ln.closedOnStopCh)
	ln.log.Info("done stopping network")
	return errs.Err
}

// Sends a SIGTERM to the given node and removes it from this network
func (ln *localNetwork) RemoveNode(nodeName string) error {
	ln.lock.Lock()
	defer ln.lock.Unlock()

	return ln.removeNode(nodeName)
}

// Assumes [net.lock] is held
func (ln *localNetwork) removeNode(nodeName string) error {
	if ln.isStopped() {
		return network.ErrStopped
	}
	ln.log.Debug("removing node %q", nodeName)
	node, ok := ln.nodes[nodeName]
	if !ok {
		return fmt.Errorf("node %q not found", nodeName)
	}
	delete(ln.nodes, nodeName)
	// cchain eth api uses a websocket connection and must be closed before stopping the node,
	// to avoid errors logs at client
	node.client.CChainEthAPI().Close()
	if err := node.process.Stop(); err != nil {
		return fmt.Errorf("error sending SIGTERM to node %s: %w", nodeName, err)
	}
	if err := node.process.Wait(); err != nil {
		return fmt.Errorf("node %q stopped with error: %w", nodeName, err)
	}
	return nil
}

// Assumes [net.lock] is held
func (ln *localNetwork) isStopped() bool {
	select {
	case <-ln.closedOnStopCh:
		return true
	default:
		return false
	}
}

// createFile creates a file with the given path and
// writes the given contents
func createFileAndWrite(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	if _, err := file.Write(contents); err != nil {
		return err
	}
	return nil
}
