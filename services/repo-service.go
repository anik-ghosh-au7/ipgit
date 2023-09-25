package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/anik-ghosh-au7/ipgit/models"
	icore "github.com/ipfs/boxo/coreiface"
	icorepath "github.com/ipfs/boxo/coreiface/path"
	"github.com/ipfs/boxo/files"
)

type RepoService struct {
	ipfs *IPFSService
}

func NewRepoService(ipfsSvc *IPFSService) *RepoService {
	return &RepoService{
		ipfs: ipfsSvc,
	}
}

var examplePeers = []string{
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

// Initialize the IPFS repo
func (service *RepoService) Init() error {
	repoPath, err := service.ipfs.CreateRepo()
	if err != nil {
		return fmt.Errorf("failed to initialize repo: %w", err)
	}

	if err := os.WriteFile(filepath.Join(repoPath, "commit_log.json"), []byte("[]"), 0644); err != nil {
		return fmt.Errorf("failed to write commit log: %w", err)
	}
	return nil
}

// Load previous commits from the commit log
func (service *RepoService) LoadCommits() ([]*models.Commit, error) {
	var commits []*models.Commit
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
func (service *RepoService) AppendToLog(newCommit *models.Commit) error {
	commits, err := service.LoadCommits()
	if err != nil {
		return err
	}
	commits = append([]*models.Commit{newCommit}, commits...)
	data, err := json.Marshal(commits)
	if err != nil {
		return err
	}
	return os.WriteFile(".ipgit/commit_log.json", data, 0644)
}

// Ensure that the staging file exists
func (service *RepoService) EnsureStagingExists() error {
	if _, err := os.Stat(".ipgit/staging.json"); os.IsNotExist(err) {
		return os.WriteFile(".ipgit/staging.json", []byte("{}"), 0644)
	}
	return nil
}

// NewSerialDir constructs a new files.Directory from the folder at the given path.
func (service *RepoService) NewSerialDir(path string) (files.Directory, error) {
	var entries []files.DirEntry

	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	// Read the directory
	fileInfos, err := dir.Readdir(-1)
	if err != nil {
		return nil, err
	}

	for _, fileInfo := range fileInfos {
		fullPath := filepath.Join(path, fileInfo.Name())

		if fileInfo.IsDir() {
			// If it's a directory, recursively create a DirEntry for it
			subDirNode, err := service.NewSerialDir(fullPath)
			if err != nil {
				return nil, err
			}
			entries = append(entries, files.FileEntry(fileInfo.Name(), subDirNode))
		} else {
			// If it's a file, create a FileEntry for it
			file, err := os.Open(fullPath)
			if err != nil {
				return nil, err
			}
			// IPFS will automatically close the file once it's done reading.
			entries = append(entries, files.FileEntry(fileInfo.Name(), files.NewReaderFile(file)))
		}
	}

	return files.NewSliceDirectory(entries), nil
}

// Add a file or folder to IPFS and the CID to the staging area
func (service *RepoService) Add(path string, ipfs icore.CoreAPI, ctx context.Context) error {
	var cidFile icorepath.Resolved

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		// If it's a directory, use the NewSerialDir function to get a directory node
		dirNode, err := service.NewSerialDir(path)
		if err != nil {
			return err
		}
		cidFile, err = ipfs.Unixfs().Add(ctx, dirNode)
		if err != nil {
			return err
		}
		// Recursively add the contents of the directory to staging.json
		err = service.AddDirToStaging(path, cidFile.Cid().String(), dirNode)
		if err != nil {
			return err
		}
	} else {
		// If it's a file, use the current logic
		fileNode, err := service.ipfs.GetUnixfsNode(path)
		if err != nil {
			return err
		}
		cidFile, err = ipfs.Unixfs().Add(ctx, fileNode)
		if err != nil {
			return err
		}
		// Add the file's CID to the staging map and write it back
		err = service.AddPathToStaging(path, cidFile.Cid().String())
		if err != nil {
			return err
		}
	}
	return nil
}

// addPathToStaging adds a given path and its CID to the staging.json
func (service *RepoService) AddPathToStaging(path, cid string) error {
	// Ensure staging.json exists
	if err := service.EnsureStagingExists(); err != nil {
		return err
	}

	// Read the existing staging data
	data, err := os.ReadFile(".ipgit/staging.json")
	if err != nil {
		return err
	}
	staging := make(map[string]string)
	if err := json.Unmarshal(data, &staging); err != nil {
		return err
	}

	// Add the new file's or directory's CID to the staging map and write it back
	staging[path] = cid
	newData, err := json.Marshal(staging)
	if err != nil {
		return err
	}
	return os.WriteFile(".ipgit/staging.json", newData, 0644)
}

// addDirToStaging recursively adds the contents of a directory to staging.json
func (service *RepoService) AddDirToStaging(basePath, baseCID string, dir files.Directory) error {
	// Ensure staging.json exists
	if err := service.EnsureStagingExists(); err != nil {
		return err
	}

	// Add the base directory itself to the staging
	err := service.AddPathToStaging(basePath, baseCID)
	if err != nil {
		return err
	}

	// Recursively add all child nodes (files/dirs) to staging
	it := dir.Entries()
	for it.Next() {
		childPath := filepath.Join(basePath, it.Name())
		switch entry := it.Node().(type) {
		case files.Directory:
			// If it's a directory, recurse into it
			err := service.AddDirToStaging(childPath, baseCID, entry)
			if err != nil {
				return err
			}
		case files.File:
			// If it's a file, simply add its path to staging
			err := service.AddPathToStaging(childPath, baseCID)
			if err != nil {
				return err
			}
		}
	}
	return it.Err()
}

// Commit changes
func (service *RepoService) Commit(ipfs icore.CoreAPI, message string, ctx context.Context) error {
	// Ensure staging.json exists before trying to read it
	if err := service.EnsureStagingExists(); err != nil {
		return err
	}
	// Load previous commits
	prevCommits, err := service.LoadCommits()
	if err != nil {
		return err
	}
	// Initialize a new commit
	newCommit := &models.Commit{
		Message:   message,
		Timestamp: time.Now(),
		Files:     make(map[string]string),
	}
	if len(prevCommits) > 0 {
		newCommit.ParentCID = prevCommits[0].CID
	}
	// Read the staging area
	staging, err := os.ReadFile(".ipgit/staging.json")
	if err != nil {
		return err
	}
	err = json.Unmarshal(staging, &newCommit.Files)
	if err != nil {
		return err
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
	return service.AppendToLog(newCommit)
}

// Check the status of the repo
func (service *RepoService) Status(ipfs icore.CoreAPI, ctx context.Context) error {
	latestCommit, err := service.LoadCommits()
	if err != nil || len(latestCommit) == 0 {
		return fmt.Errorf("no commits found")
	}
	for file, cid := range latestCommit[0].Files {
		fileNode, err := service.ipfs.GetUnixfsNode(file)
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
func (service *RepoService) Log() error {
	commits, err := service.LoadCommits()
	if err != nil {
		return err
	}
	for _, commit := range commits {
		fmt.Printf("CID: %s\nMessage: %s\nTimestamp: %s\n\n", commit.CID, commit.Message, commit.Timestamp)
	}
	return nil
}

// Show the diff of a file between commits
func (service *RepoService) Diff(ipfs icore.CoreAPI, file string, ctx context.Context) error {
	commits, err := service.LoadCommits()
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
	latestContent, err := service.ipfs.ReadUnixfsContent(latestNode)
	if err != nil {
		return err
	}
	prevContent, err := service.ipfs.ReadUnixfsContent(prevNode)
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
func (service *RepoService) Clone(ipfs icore.CoreAPI, cid string, peers []string, ctx context.Context) error {
	if err := service.ipfs.ConnectToPeers(ctx, ipfs, examplePeers); err != nil {
		return fmt.Errorf("failed to connect to peers: %s", err)
	}
	// For demonstration, let's assume cloning involves getting content by CID.
	// This would involve more than just printing content, like re-creating files, commit history, etc.
	contentNode, err := ipfs.Unixfs().Get(ctx, icorepath.New(cid))
	if err != nil {
		return err
	}
	content, err := service.ipfs.ReadUnixfsContent(contentNode)
	if err != nil {
		return err
	}
	fmt.Printf("Cloned repo content (for demonstration): %s\n", content)
	return nil
}

// executeCommand handles the execution of ipgit commands
func (service *RepoService) ExecuteCommand(cmd string, options []string, ipfs icore.CoreAPI, ctx context.Context) error {
	// Check the number of options for commands that require at least one option
	if len(options) < 1 && (cmd == "add" || cmd == "commit" || cmd == "diff" || cmd == "clone") {
		return fmt.Errorf("please specify the required argument for %s", cmd)
	}
	// Switch case to handle each command
	switch cmd {
	case "init":
		return service.Init()
	case "add":
		return service.Add(options[0], ipfs, ctx)
	case "commit":
		return service.Commit(ipfs, options[0], ctx)
	case "status":
		return service.Status(ipfs, ctx)
	case "log":
		return service.Log()
	case "diff":
		if len(options) < 1 {
			return fmt.Errorf("please specify a file to diff")
		}
		return service.Diff(ipfs, options[0], ctx)
	case "clone":
		if len(options) < 1 {
			return fmt.Errorf("please specify a CID to clone")
		}
		return service.Clone(ipfs, options[0], examplePeers, ctx)
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}
