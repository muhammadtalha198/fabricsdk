// Command events listens to chaincode events with automatic reconnect and a
// file checkpoint, so a restart resumes exactly where it stopped.
//
// Use the same environment variables as examples/basic, then run
// `go run ./examples/events`. Stop with Ctrl+C.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/muhammadtalha198/fabricsdk"
)

func main() {
	fc, err := fabricsdk.New(fabricsdk.Config{
		PeerEndpoint:     mustEnv("PEER_ENDPOINT"),
		PeerHostOverride: os.Getenv("PEER_HOST_OVERRIDE"),
		TLSCertPath:      mustEnv("TLS_CERT_PATH"),
		CertPath:         mustEnv("CERT_PATH"),
		KeyPath:          mustEnv("KEY_PATH"),
		MSPID:            mustEnv("MSP_ID"),
		ChannelName:      mustEnv("CHANNEL"),
		ChaincodeName:    mustEnv("CHAINCODE"),
	})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer fc.Close()

	cp, err := fabricsdk.NewFileCheckpointer("checkpoint.json")
	if err != nil {
		log.Fatalf("checkpoint: %v", err)
	}
	defer cp.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("listening from block %d (tx %q)\n", cp.BlockNumber(), cp.TransactionID())
	err = fc.ListenChaincodeEvents(ctx, cp, func(_ context.Context, ev fabricsdk.ChaincodeEvent) error {
		fmt.Printf("block=%d tx=%s event=%s payload=%s\n", ev.BlockNumber, ev.TxID, ev.EventName, ev.Payload)
		return nil
	}, fabricsdk.FromBlock(0))
	if err != nil {
		log.Fatalf("listener stopped: %v", err)
	}
	fmt.Println("stopped")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
