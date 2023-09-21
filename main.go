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
	repoPath, err := os.MkdirTemp("./repo", "ipfs-shell")
	if err != nil {
		return "", fmt.Errorf("failed to get temp dir: %s", err)
	}
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
func initRepo() error {
	if _, err := os.Stat(".ipgit"); !os.IsNotExist(err) {
		return fmt.Errorf(".ipgit directory already exists")
	}
	err := os.Mkdir(".ipgit", 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(".ipgit/commit_log.json", []byte("[]"), 0644)
	if err != nil {
		return err
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
	prevCommits, err := loadCommits()
	if err != nil {
		return err
	}
	newCommit := Commit{
		Message:   message,
		Timestamp: time.Now(),
		Files:     make(map[string]string),
	}
	if len(prevCommits) > 0 {
		newCommit.ParentCID = prevCommits[0].CID
	}
	files, _ := filepath.Glob(".ipgit/staging_*")
	for _, file := range files {
		cid, _ := os.ReadFile(file)
		newCommit.Files[strings.TrimPrefix(file, ".ipgit/staging_")] = string(cid)
		if err := os.Remove(file); err != nil {
			return err
		}
	}
	commitBytes, err := json.Marshal(newCommit)
	if err != nil {
		return err
	}
	cid, err := ipfs.Block().Put(ctx, bytes.NewReader(commitBytes))
	if err != nil {
		return err
	}
	newCommit.CID = cid.Path().String()
	// Error handling for appendToLog
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
func clone(ipfs icore.CoreAPI, cid string, ctx context.Context) error {
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

func main() {
	// Initialize an IPFS node (this is simplified; see your original example for a full version)
	r, err := fsrepo.Open("~/.ipfs")
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &core.BuildCfg{
		Repo: r,
	}

	node, err := core.NewNode(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}

	ipfs, err := coreapi.NewCoreAPI(node)
	if err != nil {
		log.Fatal(err)
	}

	// Assume the command is the first argument and options are the rest
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("Available commands: init, add, commit, status, log, diff, clone")
		return
	}

	cmd := args[0]
	options := args[1:]

	// Allow the init command to run even if .ipgit directory exists
	if cmd != "init" {
		if _, err := os.Stat(".ipgit"); os.IsNotExist(err) {
			fmt.Println("Not an ipgit repository. Please initialize the repository using 'ipgit init'")
			return
		}
	}

	switch cmd {
	case "init":
		err = initRepo()
	case "add":
		if len(options) < 1 {
			log.Fatal("Please specify a file to add")
		}
		err = addFile(options[0], ipfs, ctx)
	case "commit":
		if len(options) < 1 {
			log.Fatal("Please specify a commit message")
		}
		err = commit(ipfs, options[0], ctx)
	case "status":
		err = status(ipfs, ctx)
	case "log":
		err = viewLog()
	case "diff":
		if len(options) < 1 {
			log.Fatal("Please specify a file to diff")
		}
		err = diff(ipfs, options[0], ctx)
	case "clone":
		if len(options) < 1 {
			log.Fatal("Please specify a CID to clone")
		}
		err = clone(ipfs, options[0], ctx)
	default:
		log.Fatalf("Unknown command: %s", cmd)
	}

	if err != nil {
		log.Fatal(err)
	}
}
