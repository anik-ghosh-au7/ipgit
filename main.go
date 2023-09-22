package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	icore "github.com/ipfs/boxo/coreiface"
	icorepath "github.com/ipfs/boxo/coreiface/path"
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

type Commit struct {
	CID       string            `json:"cid"`
	ParentCID string            `json:"parent_cid"`
	Message   string            `json:"message"`
	Timestamp time.Time         `json:"timestamp"`
	Files     map[string]string `json:"files"`
}

type IPFSService struct {
	experimental bool
	pluginOnce   sync.Once
}

func NewIPFSService(experimental bool) *IPFSService {
	return &IPFSService{
		experimental: experimental,
	}
}

func (service *IPFSService) setupPlugins(externalPluginsPath string) error {
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

func (service *IPFSService) createTempRepo() (string, error) {
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

func (service *IPFSService) createNode(ctx context.Context, repoPath string) (*core.IpfsNode, error) {
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

func (service *IPFSService) spawnEphemeral(ctx context.Context) (icore.CoreAPI, *core.IpfsNode, error) {
	var onceErr error
	service.pluginOnce.Do(func() {
		onceErr = service.setupPlugins("")
	})
	if onceErr != nil {
		return nil, nil, onceErr
	}
	repoPath, err := service.createTempRepo()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temp repo: %s", err)
	}
	node, err := service.createNode(ctx, repoPath)
	if err != nil {
		return nil, nil, err
	}
	api, err := coreapi.NewCoreAPI(node)
	return api, node, err
}

func connectToPeers(ctx context.Context, ipfs icore.CoreAPI, peers []string) error {
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

func getUnixfsNode(path string) (files.Node, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return files.NewSerialFile(path, false, st)
}

// Initialize the IPFS repo
func (service *IPFSService) initRepo() error {
	repoPath, err := service.createTempRepo()
	if err != nil {
		return fmt.Errorf("failed to initialize repo: %w", err)
	}

	if err := os.WriteFile(filepath.Join(repoPath, "commit_log.json"), []byte("[]"), 0644); err != nil {
		return fmt.Errorf("failed to write commit log: %w", err)
	}
	return nil
}

// Load previous commits from the commit log
func loadCommits() ([]Commit, error) {
	var commits []Commit
	data, err := os.ReadFile(".ipgit/commit_log.json")
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, &commits)
	if err != nil {
		return nil, err
	}
	return commits, nil
}

// Append a new commit to the commit log
func appendToLog(newCommit Commit) error {
	commits, err := loadCommits()
	if err != nil {
		return err
	}
	commits = append([]Commit{newCommit}, commits...)
	data, err := json.Marshal(commits)
	if err != nil {
		return err
	}
	return os.WriteFile(".ipgit/commit_log.json", data, 0644)
}

// Add a file to IPFS and the CID to the staging area
func addFile(file string, ipfs icore.CoreAPI, ctx context.Context) error {
	fileNode, err := getUnixfsNode(file)
	if err != nil {
		return err
	}
	cidFile, err := ipfs.Unixfs().Add(ctx, fileNode)
	if err != nil {
		return err
	}
	// Error handling for WriteFile
	if err := os.WriteFile(".ipgit/staging_"+file, []byte(cidFile.Cid().String()), 0644); err != nil {
		return err
	}
	return nil
}

// Commit changes
func commit(ipfs icore.CoreAPI, message string, ctx context.Context) error {
	// Load previous commits
	prevCommits, err := loadCommits()
	if err != nil {
		return err
	}
	// Initialize a new commit
	newCommit := Commit{
		Message:   message,
		Timestamp: time.Now(),
		Files:     make(map[string]string),
	}
	if len(prevCommits) > 0 {
		newCommit.ParentCID = prevCommits[0].CID
	}
	// Read the staging area
	files, err := filepath.Glob(".ipgit/staging_*")
	if err != nil {
		return err // Added error handling
	}
	for _, file := range files {
		cid, err := os.ReadFile(file)
		if err != nil {
			return err // Added error handling
		}
		newCommit.Files[strings.TrimPrefix(file, ".ipgit/staging_")] = string(cid)
		if err := os.Remove(file); err != nil {
			return err // Added error handling
		}
	}
	// Serialize the commit to JSON
	commitBytes, err := json.Marshal(newCommit)
	if err != nil {
		return err
	}
	// Add the commit to IPFS
	cid, err := ipfs.Block().Put(ctx, bytes.NewReader(commitBytes))
	if err != nil {
		return err
	}
	newCommit.CID = cid.Path().String()
	// Append the new commit to the commit log
	if err := appendToLog(newCommit); err != nil {
		return err
	}
	return appendToLog(newCommit)
}

// Check the status of the repo
func status(ipfs icore.CoreAPI, ctx context.Context) error {
	latestCommit, err := loadCommits()
	if err != nil || len(latestCommit) == 0 {
		return fmt.Errorf("no commits found")
	}
	for file, cid := range latestCommit[0].Files {
		fileNode, err := getUnixfsNode(file)
		if err != nil {
			return err
		}
		currentCID, err := ipfs.Unixfs().Add(ctx, fileNode)
		if err != nil {
			return err
		}
		if currentCID.Cid().String() != cid {
			fmt.Printf("Modified: %s\n", file)
		}
	}
	return nil
}

// View the commit log
func viewLog() error {
	commits, err := loadCommits()
	if err != nil {
		return err
	}
	for _, commit := range commits {
		fmt.Printf("CID: %s\nMessage: %s\nTimestamp: %s\n\n", commit.CID, commit.Message, commit.Timestamp)
	}
	return nil
}

// Show the diff of a file between commits
func diff(ipfs icore.CoreAPI, file string, ctx context.Context) error {
	commits, err := loadCommits()
	if err != nil {
		return err
	}
	if len(commits) < 2 {
		return fmt.Errorf("not enough commits for diff")
	}
	latestCID := commits[0].Files[file]
	prevCID := commits[1].Files[file]
	latestNode, err := ipfs.Unixfs().Get(ctx, icorepath.New(latestCID))
	if err != nil {
		return err
	}
	prevNode, err := ipfs.Unixfs().Get(ctx, icorepath.New(prevCID))
	if err != nil {
		return err
	}
	latestContent, err := readUnixfsContent(latestNode)
	if err != nil {
		return err
	}
	prevContent, err := readUnixfsContent(prevNode)
	if err != nil {
		return err
	}
	if bytes.Equal(latestContent.Bytes(), prevContent.Bytes()) {
		fmt.Println("No changes.")
	} else {
		fmt.Println("Files differ.")
	}
	return nil
}

// Clone a repo by its CID
func clone(ipfs icore.CoreAPI, cid string, peers []string, ctx context.Context) error {
	if err := connectToPeers(ctx, ipfs, peers); err != nil {
		return fmt.Errorf("failed to connect to peers: %s", err)
	}
	// For demonstration, let's assume cloning involves getting content by CID.
	contentNode, err := ipfs.Unixfs().Get(ctx, icorepath.New(cid))
	if err != nil {
		return err
	}
	content, err := readUnixfsContent(contentNode)
	if err != nil {
		return err
	}
	// Assume content holds repo data, and reconstruct the repo from it.
	// This would involve more than just printing content, like re-creating files, commit history, etc.
	fmt.Printf("Cloned repo content (for demonstration): %s\n", content)
	return nil
}

// Read content from Unixfs node into bytes.Buffer
func readUnixfsContent(node files.Node) (*bytes.Buffer, error) {
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

// executeCommand handles the execution of ipgit commands
func executeCommand(cmd string, options []string, ipfs icore.CoreAPI, service *IPFSService, ctx context.Context) error {
	// Check the number of options for commands that require at least one option
	if len(options) < 1 && (cmd == "add" || cmd == "commit" || cmd == "diff" || cmd == "clone") {
		return fmt.Errorf("please specify the required argument for %s", cmd)
	}
	// Switch case to handle each command
	switch cmd {
	case "init":
		return service.initRepo()
	case "add":
		return addFile(options[0], ipfs, ctx)
	case "commit":
		return commit(ipfs, options[0], ctx)
	case "status":
		return status(ipfs, ctx)
	case "log":
		return viewLog()
	case "diff":
		if len(options) < 1 {
			return fmt.Errorf("please specify a file to diff")
		}
		return diff(ipfs, options[0], ctx)
	case "clone":
		if len(options) < 1 {
			return fmt.Errorf("please specify a CID to clone")
		}
		examplePeers := []string{
			// IPFS Bootstrapper nodes
			"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
			"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
			"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
			"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
			// IPFS Cluster Pinning nodes
			"/ip4/138.201.67.219/tcp/4001/p2p/QmUd6zHcbkbcs7SMxwLs48qZVX3vpcM8errYS7xEczwRMA",
			"/ip4/138.201.67.219/udp/4001/quic/p2p/QmUd6zHcbkbcs7SMxwLs48qZVX3vpcM8errYS7xEczwRMA",
			"/ip4/138.201.67.220/tcp/4001/p2p/QmNSYxZAiJHeLdkBg38roksAR9So7Y5eojks1yjEcUtZ7i",
			"/ip4/138.201.67.220/udp/4001/quic/p2p/QmNSYxZAiJHeLdkBg38roksAR9So7Y5eojks1yjEcUtZ7i",
			"/ip4/138.201.68.74/tcp/4001/p2p/QmdnXwLrC8p1ueiq2Qya8joNvk3TVVDAut7PrikmZwubtR",
			"/ip4/138.201.68.74/udp/4001/quic/p2p/QmdnXwLrC8p1ueiq2Qya8joNvk3TVVDAut7PrikmZwubtR",
			"/ip4/94.130.135.167/tcp/4001/p2p/QmUEMvxS2e7iDrereVYc5SWPauXPyNwxcy9BXZrC1QTcHE",
			"/ip4/94.130.135.167/udp/4001/quic/p2p/QmUEMvxS2e7iDrereVYc5SWPauXPyNwxcy9BXZrC1QTcHE",
		}

		return clone(ipfs, options[0], examplePeers, ctx)
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func main() {
	// Create a context that you can cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Cancel the context when main returns

	// Parse command-line arguments
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("Available commands: init, add, commit, status, log, diff, clone")
		return
	}

	cmd := args[0]
	options := args[1:]

	// Create an IPFSService instance
	ipfsService := NewIPFSService(false) // Set experimental to true if needed

	// Declare a variable for the CoreAPI
	var ipfs icore.CoreAPI

	// Always create an ephemeral node for this example
	ipfs, _, err := ipfsService.spawnEphemeral(ctx)
	if err != nil {
		log.Fatal("Failed to spawn ephemeral IPFS node: ", err)
	}

	// Execute the command
	if err := executeCommand(cmd, options, ipfs, ipfsService, ctx); err != nil {
		log.Fatal(err)
	}
}
