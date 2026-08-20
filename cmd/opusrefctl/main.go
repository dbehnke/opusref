// Command opusrefctl sends and receives diagnostic OpusRef records.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/dbehnke/opusref/internal/record"
	"github.com/dbehnke/opusref/pkg/client"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type common struct {
	server, node, keyEnv, keyFile    string
	connectTimeout, operationTimeout time.Duration
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) == 0 {
		return errors.New("use listen or transmit")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	var c common
	fs.StringVar(&c.server, "server", "", "reflector host:port")
	fs.StringVar(&c.node, "node", "", "node callsign")
	fs.StringVar(&c.keyEnv, "shared-key-env", "OPUSREF_SHARED_KEY", "shared key environment variable")
	fs.StringVar(&c.keyFile, "shared-key-file", "", "shared key file")
	fs.DurationVar(&c.connectTimeout, "connect-timeout", 10*time.Second, "connection timeout")
	fs.DurationVar(&c.operationTimeout, "operation-timeout", 5*time.Second, "operation timeout")
	source := ""
	if args[0] == "transmit" {
		fs.StringVar(&source, "source", "", "source callsign")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if c.server == "" || c.node == "" {
		return errors.New("--server and --node are required")
	}
	if args[0] == "transmit" && source == "" {
		return errors.New("--source is required")
	}
	if args[0] != "listen" && args[0] != "transmit" {
		return errors.New("use listen or transmit")
	}
	key, err := readKey(c)
	if err != nil {
		return err
	}
	cli, err := client.NewUDP(client.Options{ServerAddress: c.server, NodeCallsign: c.node, SharedKey: key, ConnectTimeout: c.connectTimeout, OperationTimeout: c.operationTimeout})
	if err != nil {
		return err
	}
	defer cli.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancel := context.WithTimeout(ctx, c.connectTimeout)
	err = cli.Connect(connectCtx)
	cancel()
	if err != nil {
		return err
	}
	select {
	case <-cli.Events():
	case <-ctx.Done():
		return nil
	}
	if args[0] == "listen" {
		return listen(ctx, cli, out)
	}
	opCtx, cancel := context.WithTimeout(ctx, c.operationTimeout)
	err = cli.RequestStream(opCtx, source)
	cancel()
	if err != nil {
		return err
	}
	for {
		rec, err := record.Read(in)
		if errors.Is(err, io.EOF) {
			opCtx, cancel = context.WithTimeout(ctx, c.operationTimeout)
			err = cli.EndStream(opCtx)
			cancel()
			return err
		}
		if err != nil {
			return err
		}
		if rec.Kind == record.KindAudio {
			err = cli.SendAudio(ctx, rec.Timestamp, rec.Payload)
		} else {
			err = cli.SendData(ctx, rec.Timestamp, rec.DataType, rec.Payload)
		}
		if err != nil {
			return err
		}
	}
}
func listen(ctx context.Context, cli client.Client, out io.Writer) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-cli.Done():
			return cli.Err()
		case event := <-cli.Events():
			switch event.Kind {
			case client.EventAudio:
				if err := record.Write(out, record.Record{Kind: record.KindAudio, Timestamp: event.Timestamp, Payload: event.Payload}); err != nil {
					return err
				}
			case client.EventData:
				if err := record.Write(out, record.Record{Kind: record.KindData, DataType: event.DataType, Timestamp: event.Timestamp, Payload: event.Payload}); err != nil {
					return err
				}
			}
		}
	}
}
func readKey(c common) (string, error) {
	if c.keyEnv != "" {
		if value := os.Getenv(c.keyEnv); value != "" {
			if len(value) < 16 || len(value) > 64 {
				return "", errors.New("shared key length must be 16 through 64 bytes")
			}
			return value, nil
		}
	}
	if c.keyFile == "" {
		return "", nil
	}
	info, err := os.Stat(c.keyFile)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("shared key file permissions are not restricted")
	}
	data, err := os.ReadFile(c.keyFile)
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if len(value) < 16 || len(value) > 64 {
		return "", errors.New("shared key length must be 16 through 64 bytes")
	}
	return value, nil
}
