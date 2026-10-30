// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main provides an example SideroLink server which configures Talos Linux nodes.
//
// Nodes connect to the server over SideroLink (WireGuard). For every node that connects, the server
// dials the node's Talos API over the tunnel, queries the node for hardware identifiers, network links
// and LLDP neighbors, looks up matching machine configuration files in the data directory and applies
// the combined configuration to the node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/siderolabs/siderolink/pkg/agent"
	"github.com/siderolabs/siderolink/pkg/wireguard"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)

		os.Exit(1)
	}
}

func run() error {
	var (
		cfg            agent.Config
		opts           provisionerOptions
		dataDir        string
		applyMode      string
		joinConfigFile string
		debug          bool
	)

	flag.StringVar(&cfg.WireguardEndpoint, "sidero-link-wireguard-endpoint", "172.20.0.1:51821", "advertised Wireguard endpoint")
	flag.StringVar(&cfg.APIEndpoint, "sidero-link-api-endpoint", ":4000", "gRPC API endpoint for the SideroLink")
	flag.StringVar(&cfg.JoinToken, "sidero-link-join-token", "", "join token")
	flag.BoolVar(&cfg.ForceUserspace, "sidero-link-force-userspace", false, "force usage of userspace UDP device for Wireguard")
	flag.StringVar(&cfg.SinkEndpoint, "event-sink-endpoint", ":8080", "gRPC API endpoint for the Event Sink")
	flag.StringVar(&cfg.LogEndpoint, "log-receiver-endpoint", ":4001", "TCP log receiver endpoint")

	flag.StringVar(&dataDir, "data-dir", "./data", "directory with machine configuration files")
	flag.StringVar(&opts.CommonConfig, "common-config", "", "configuration file combined with every node's configuration (default: <data-dir>/common.yaml)")
	flag.StringVar(&opts.DefaultConfig, "default-config", "", "configuration file used when no identifier matches (default: <data-dir>/default.yaml)")
	flag.StringVar(&applyMode, "apply-mode", "auto", "configuration apply mode: auto, no-reboot or staged")
	flag.BoolVar(&opts.DryRun, "dry-run", false, "query the nodes and log the configuration that would be applied without applying it")
	flag.DurationVar(&opts.ConnectTimeout, "connect-timeout", 2*time.Minute, "how long to wait for the node's Talos API to become reachable over SideroLink")
	flag.DurationVar(&opts.LLDPWait, "lldp-wait", 45*time.Second, "how long to wait for LLDP neighbors to be observed before matching configuration")
	flag.StringVar(&opts.JoinAPIURL, "join-api-url", "",
		"SideroLink API URL the nodes use to join, as reachable from the nodes, e.g. grpc://172.20.0.1:4000 "+
			"(the join token is added automatically); when set, the join configuration is printed on startup "+
			"and appended to every applied configuration which lacks a SideroLinkConfig document, so that nodes stay connected after reboot")
	flag.StringVar(&joinConfigFile, "join-config-file", "", "write the join configuration to this file on startup (requires -join-api-url)")
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	flag.Parse()

	var err error

	opts.ApplyMode, err = parseApplyMode(applyMode)
	if err != nil {
		return err
	}

	opts.Store, err = newConfigStore(dataDir, opts.CommonConfig, opts.DefaultConfig)
	if err != nil {
		return err
	}

	// The server's own address in the SideroLink network: nodes reach the event sink and the log receiver via it.
	nodePrefix := wireguard.NetworkPrefix("")
	opts.ServerAddress = nodePrefix.Addr().Next()

	if opts.SinkPort, err = portOf(cfg.SinkEndpoint); err != nil {
		return fmt.Errorf("invalid event sink endpoint: %w", err)
	}

	if opts.LogPort, err = portOf(cfg.LogEndpoint); err != nil {
		return fmt.Errorf("invalid log receiver endpoint: %w", err)
	}

	if opts.JoinAPIURL != "" {
		if opts.JoinAPIURL, err = withJoinToken(opts.JoinAPIURL, cfg.JoinToken); err != nil {
			return err
		}
	}

	logCfg := zap.NewDevelopmentConfig()
	logCfg.DisableStacktrace = true

	if !debug {
		logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}

	logger, err := logCfg.Build()
	if err != nil {
		return fmt.Errorf("error creating logger: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if opts.JoinAPIURL != "" {
		joinConfig, err := joinConfigDocuments(opts)
		if err != nil {
			return err
		}

		logger.Info("join configuration for the nodes:\n" + string(joinConfig))

		if joinConfigFile != "" {
			if err = os.WriteFile(joinConfigFile, joinConfig, 0o600); err != nil {
				return fmt.Errorf("error writing join configuration: %w", err)
			}

			logger.Info("join configuration written", zap.String("file", joinConfigFile))
		}
	} else if joinConfigFile != "" {
		return errors.New("-join-config-file requires -join-api-url")
	}

	p := newProvisioner(ctx, opts, logger)
	defer p.Wait()

	logger.Info("remote config server starting",
		zap.String("data_dir", dataDir),
		zap.Stringer("apply_mode", opts.ApplyMode),
		zap.Bool("dry_run", opts.DryRun),
		zap.Stringer("server_address", opts.ServerAddress),
	)

	return agent.Run(ctx, cfg, p, logger)
}

func parseApplyMode(mode string) (machineapi.ApplyConfigurationRequest_Mode, error) {
	switch strings.ToLower(mode) {
	case "auto":
		return machineapi.ApplyConfigurationRequest_AUTO, nil
	case "no-reboot":
		return machineapi.ApplyConfigurationRequest_NO_REBOOT, nil
	case "staged":
		return machineapi.ApplyConfigurationRequest_STAGED, nil
	default:
		return 0, fmt.Errorf("unknown apply mode %q: expected auto, no-reboot or staged", mode)
	}
}

// withJoinToken adds the join token to the API URL unless the URL already carries one.
func withJoinToken(apiURL, joinToken string) (string, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return "", fmt.Errorf("invalid join API URL: %w", err)
	}

	if u.Scheme != "grpc" && u.Scheme != "https" {
		return "", errors.New("join API URL scheme must be grpc:// (plain text) or https://")
	}

	if joinToken != "" && !u.Query().Has("jointoken") {
		q := u.Query()
		q.Set("jointoken", joinToken)
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}

func portOf(endpoint string) (string, error) {
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", err
	}

	return port, nil
}
