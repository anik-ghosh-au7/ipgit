package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	icore "github.com/ipfs/boxo/coreiface"
	"github.com/ipfs/boxo/files"
	"github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/core/coreapi"
	"github.com/ipfs/kubo/core/node/libp2p"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo/fsrepo"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

type IPFSService struct {
	experimental bool
	pluginOnce   sync.Once
}

func NewIPFSService(experimental bool) *IPFSService {
	return &IPFSService{
		experimental: experimental,
	}
}

func (service *IPFSService) SetupPlugins(externalPluginsPath string) error {
	plugins, err := loader.NewPluginLoader(filepath.Join(externalPluginsPath, "plugins"))
	if err != nil {
		return fmt.Errorf("error loading plugins: %s", err)
	}
	if err := plugins.Initialize(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}
	if err := plugins.Inject(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}
	return nil
}

func (service *IPFSService) CreateRepo() (string, error) {
	// Ensure that .ipgit exists
	if _, err := os.Stat(".ipgit"); os.IsNotExist(err) {
		err := os.MkdirAll(".ipgit", 0755)
		if err != nil {
			return "", fmt.Errorf("failed to create repo directory: %s", err)
		}
	}

	repoPath := ".ipgit" // Use .ipgit as the repo path

	// Check if the repo is already initialized, if not then initialize it
	if !fsrepo.IsInitialized(repoPath) {
		cfg, err := config.Init(io.Discard, 2048)
		if err != nil {
			return "", err
		}
		if service.experimental {
			cfg.Experimental.FilestoreEnabled = true
			cfg.Experimental.UrlstoreEnabled = true
			cfg.Experimental.Libp2pStreamMounting = true
			cfg.Experimental.P2pHttpProxy = true
		}
		err = fsrepo.Init(repoPath, cfg)
		if err != nil {
			return "", fmt.Errorf("failed to init ephemeral node: %s", err)
		}
	}

	return repoPath, nil
}

func (service *IPFSService) CreateNode(ctx context.Context, repoPath string) (*core.IpfsNode, error) {
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, err
	}
	nodeOptions := &core.BuildCfg{
		Online:  true,
		Routing: libp2p.DHTOption,
		Repo:    repo,
	}
	return core.NewNode(ctx, nodeOptions)
}

func (service *IPFSService) SpawnEphemeral(ctx context.Context) (icore.CoreAPI, *core.IpfsNode, error) {
	var onceErr error
	service.pluginOnce.Do(func() {
		onceErr = service.SetupPlugins("")
	})
	if onceErr != nil {
		return nil, nil, onceErr
	}
	repoPath, err := service.CreateRepo()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temp repo: %s", err)
	}
	node, err := service.CreateNode(ctx, repoPath)
	if err != nil {
		return nil, nil, err
	}
	api, err := coreapi.NewCoreAPI(node)
	return api, node, err
}

func (service *IPFSService) ConnectToPeers(ctx context.Context, ipfs icore.CoreAPI, peers []string) error {
	var wg sync.WaitGroup
	peerInfos := make(map[peer.ID]*peer.AddrInfo, len(peers))
	for _, addrStr := range peers {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			return err
		}
		pii, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			return err
		}
		pi, ok := peerInfos[pii.ID]
		if !ok {
			pi = &peer.AddrInfo{ID: pii.ID}
			peerInfos[pi.ID] = pi
		}
		pi.Addrs = append(pi.Addrs, pii.Addrs...)
	}
	wg.Add(len(peerInfos))
	for _, peerInfo := range peerInfos {
		go func(peerInfo *peer.AddrInfo) {
			defer wg.Done()
			err := ipfs.Swarm().Connect(ctx, *peerInfo)
			if err != nil {
				log.Printf("failed to connect to %s: %s", peerInfo.ID, err)
			}
		}(peerInfo)
	}
	wg.Wait()
	return nil
}

func (service *IPFSService) GetUnixfsNode(path string) (files.Node, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return files.NewSerialFile(path, false, st)
}

// Read content from Unixfs node into bytes.Buffer
func (service *IPFSService) ReadUnixfsContent(node files.Node) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	f := files.ToFile(node)
	if f == nil {
		return nil, fmt.Errorf("file type assertion failed")
	}
	_, err := io.Copy(&buf, f)
	if err != nil {
		return nil, err
	}
	return &buf, nil
}
