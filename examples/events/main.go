// Command events demonstrates chaincode event listening with fabricsdk.
//
// Configure via environment variables (same as basic example) then run:
//
//	go run main.go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	fabricsdk "github.com/muhammadtalha198/fabricsdk"
)

func main() {
	cfg := fabricsdk.Config{
		PeerEndpoint:     mustEnv("PEER_ENDPOINT"),
		PeerHostOverride: mustEnv("PEER_HOST_OVERRIDE"),
		TLSCertPath:      mustEnv("TLS_CERT_PATH"),
		CertPath:         mustEnv("CERT_PATH"),
		KeyPath:          mustEnv("KEY_PATH"),
		MSPID:            mustEnv("MSP_ID"),
		ChannelName:      mustEnv("CHANNEL"),
		ChaincodeName:    mustEnv("CHAINCODE"),
	}

	fc, err := fabricsdk.New(cfg)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer fc.Close()

	// Cancel on Ctrl+C
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Println("Listening for chaincode events. Press Ctrl+C to stop.")

	events, err := fc.Events(ctx)
	if err != nil {
		log.Fatalf("events: %v", err)
	}

	for ev := range events {
		fmt.Printf("block=%-6d event=%-20s txID=%s payload=%s\n",
			ev.BlockNumber, ev.EventName, ev.TxID[:8]+"...", ev.Payload)
	}

	fmt.Println("Event stream closed.")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
