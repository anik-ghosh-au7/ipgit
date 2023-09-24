package main

import (
	"context"
	"fmt"
	"os"

	"github.com/anik-ghosh-au7/ipgit/services"
)

func main() {
	// Create context for operations
	ctx := context.Background()

	// Initialize the IPFS service
	ipfsSvc := services.NewIPFSService(false)

	// Initialize the repo service with the IPFS service as a dependency
	repoSvc := services.NewRepoService(ipfsSvc)

	// A simple command handler, you can expand this as per your needs
	if len(os.Args) < 2 {
		fmt.Println("Please provide a command")
		return
	}

	cmd := os.Args[1]
	options := os.Args[2:]

	// For demonstration purposes, assuming the CoreAPI is required
	// You might want to adjust this based on the actual requirements of your functions
	ipfsCoreAPI, _, err := ipfsSvc.SpawnEphemeral(ctx)
	if err != nil {
		fmt.Printf("Error spawning IPFS: %s\n", err)
		return
	}

	err = repoSvc.ExecuteCommand(cmd, options, ipfsCoreAPI, ctx)
	if err != nil {
		fmt.Printf("Error executing command: %s\n", err)
	}
}
