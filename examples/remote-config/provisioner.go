// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/siderolabs/siderolink/pkg/wireguard"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"go.uber.org/zap"
	"go.yaml.in/yaml/v4"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// provisionerOptions controls how nodes are queried and configured.
type provisionerOptions struct {
	ServerAddress  netip.Addr
	Store          *configStore
	CommonConfig   string
	DefaultConfig  string
	JoinAPIURL     string
	SinkPort       string
	LogPort        string
	ConnectTimeout time.Duration
	LLDPWait       time.Duration
	ApplyMode      machineapi.ApplyConfigurationRequest_Mode
	DryRun         bool
}

// provisioner implements [wireguard.PeerHandler]: it is notified about every node (WireGuard peer)
// which joins or leaves the SideroLink network and configures the nodes which join.
type provisioner struct {
	ctx    context.Context //nolint:containedctx
	logger *zap.Logger
	nodes  map[wgtypes.Key]*nodeTask
	opts   provisionerOptions
	wg     sync.WaitGroup
	mu     sync.Mutex
}

type nodeTask struct {
	cancel  context.CancelFunc
	address netip.Addr
}

func newProvisioner(ctx context.Context, opts provisionerOptions, logger *zap.Logger) *provisioner {
	return &provisioner{
		ctx:    ctx,
		opts:   opts,
		logger: logger,
		nodes:  map[wgtypes.Key]*nodeTask{},
	}
}

// Wait blocks until all per-node tasks have finished.
func (p *provisioner) Wait() {
	p.wg.Wait()
}

// HandlePeerAdded implements [wireguard.PeerHandler].
func (p *provisioner) HandlePeerAdded(event wireguard.PeerEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if task, ok := p.nodes[event.PubKey]; ok {
		if task.address == event.Address {
			// the peer re-provisioned with the same address, let the running task continue
			return nil
		}

		task.cancel()
	}

	ctx, cancel := context.WithCancel(p.ctx)
	p.nodes[event.PubKey] = &nodeTask{cancel: cancel, address: event.Address}

	logger := p.logger.With(
		zap.Stringer("node", event.Address),
		zap.String("public_key", event.PubKey.String()),
	)

	logger.Info("node connected", zap.String("endpoint", event.Endpoint), zap.Bool("over_grpc", event.VirtualAddr.IsValid()))

	p.wg.Go(func() {
		defer cancel()

		if err := p.provision(ctx, event.Address, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("failed to configure node", zap.Error(err))
		}
	})

	return nil
}

// HandlePeerRemoved implements [wireguard.PeerHandler].
func (p *provisioner) HandlePeerRemoved(pubKey wgtypes.Key) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if task, ok := p.nodes[pubKey]; ok {
		p.logger.Info("node disconnected", zap.Stringer("node", task.address), zap.String("public_key", pubKey.String()))

		task.cancel()
		delete(p.nodes, pubKey)
	}

	return nil
}

func (p *provisioner) provision(ctx context.Context, address netip.Addr, logger *zap.Logger) error {
	c, err := p.connect(ctx, address, logger)
	if err != nil {
		return err
	}

	defer c.Close() //nolint:errcheck

	info, err := queryNode(ctx, c, address, p.opts.LLDPWait, logger)
	if err != nil {
		return fmt.Errorf("error querying node: %w", err)
	}

	infoYAML, err := yaml.Marshal(info)
	if err != nil {
		return err
	}

	logger.Info("node information:\n" + string(infoYAML))

	result, err := p.opts.Store.Lookup(info)
	if err != nil {
		return fmt.Errorf("error looking up configuration: %w", err)
	}

	if len(result.Data) == 0 {
		logger.Warn("no configuration found for the node, add files to the data directory and reconnect the node")

		return nil
	}

	data, err := finalizeConfig(result.Data, p.opts)
	if err != nil {
		return fmt.Errorf("invalid configuration for the node (matched %s): %w", strings.Join(result.Matched, ", "), err)
	}

	logger.Info("configuration assembled", zap.Strings("matched", result.Matched), zap.Int("bytes", len(data)))

	if p.opts.DryRun {
		logger.Info("dry run, configuration which would be applied:\n" + string(data))

		return nil
	}

	applyCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	resp, err := c.ApplyConfiguration(applyCtx, &machineapi.ApplyConfigurationRequest{
		Data: data,
		Mode: p.opts.ApplyMode,
	})
	if err != nil {
		return fmt.Errorf("error applying configuration: %w", err)
	}

	for _, msg := range resp.GetMessages() {
		logger.Info("configuration applied", zap.String("details", msg.GetModeDetails()), zap.Strings("warnings", msg.GetWarnings()))
	}

	return nil
}

// connect dials the Talos API of the node over SideroLink.
//
// The node might still be booting when the WireGuard peer shows up, so the connection is retried
// until the API answers or the connect timeout expires. A node which has already been configured
// requires client certificates, which are not available to this server: such nodes are skipped.
func (p *provisioner) connect(ctx context.Context, address netip.Addr, logger *zap.Logger) (*client.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ConnectTimeout)
	defer cancel()

	const retryInterval = 5 * time.Second

	for {
		c, err := client.New(ctx, client.WithMaintenanceMode(address.String(), nil))
		if err != nil {
			return nil, fmt.Errorf("error creating Talos client: %w", err)
		}

		versionCtx, versionCancel := context.WithTimeout(ctx, retryInterval)

		resp, err := c.Version(versionCtx)

		versionCancel()

		if err == nil {
			if len(resp.GetMessages()) > 0 {
				logger.Info("connected to the node", zap.String("talos_version", resp.GetMessages()[0].GetVersion().GetTag()))
			}

			return c, nil
		}

		c.Close() //nolint:errcheck

		if isClientCertRequired(err) {
			return nil, fmt.Errorf("node is not in maintenance mode (it has a machine configuration already), skipping: %w", err)
		}

		logger.Debug("node API is not reachable yet, retrying", zap.Error(err))

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("node API did not become reachable: %w (last error: %w)", ctx.Err(), err)
		case <-time.After(retryInterval):
		}
	}
}

// isClientCertRequired detects the TLS handshake failure returned by a configured node, which
// requires mutual TLS, when the client doesn't present a certificate.
func isClientCertRequired(err error) bool {
	msg := err.Error()

	return strings.Contains(msg, "tls:") && strings.Contains(msg, "certificate")
}
