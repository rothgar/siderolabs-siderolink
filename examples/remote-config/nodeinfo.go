// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"go.uber.org/zap"
)

// nodeInfo is everything the server learns about a node before configuring it.
type nodeInfo struct {
	Address      string     `yaml:"address"`
	TalosVersion string     `yaml:"talosVersion,omitempty"`
	UUID         string     `yaml:"uuid,omitempty"`
	Serial       string     `yaml:"serial,omitempty"`
	Manufacturer string     `yaml:"manufacturer,omitempty"`
	Product      string     `yaml:"product,omitempty"`
	Hostname     string     `yaml:"hostname,omitempty"`
	Links        []linkInfo `yaml:"links"`
}

// linkInfo describes a physical network link of the node and the LLDP neighbors seen on it.
type linkInfo struct {
	Name      string                     `yaml:"name"`
	MAC       string                     `yaml:"mac"`
	Neighbors []network.LLDPNeighborSpec `yaml:"neighbors,omitempty"`
	Up        bool                       `yaml:"up"`
}

// queryNode reads the node identity, links and LLDP neighbors via the Talos API.
//
// LLDP neighbors are only known once the switch has sent an advertisement (typically every 30 seconds),
// so the function waits up to lldpWait for at least one neighbor to show up.
func queryNode(ctx context.Context, c *client.Client, address netip.Addr, lldpWait time.Duration, logger *zap.Logger) (*nodeInfo, error) {
	info := &nodeInfo{Address: address.String()}

	version, err := c.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting version: %w", err)
	}

	if len(version.GetMessages()) > 0 {
		info.TalosVersion = version.GetMessages()[0].GetVersion().GetTag()
	}

	sysInfo, err := safe.StateGetByID[*hardware.SystemInformation](ctx, c.COSI, hardware.SystemInformationID)
	if err != nil && !state.IsNotFoundError(err) {
		return nil, fmt.Errorf("error getting system information: %w", err)
	}

	if sysInfo != nil {
		info.UUID = sysInfo.TypedSpec().UUID
		info.Serial = sysInfo.TypedSpec().SerialNumber
		info.Manufacturer = sysInfo.TypedSpec().Manufacturer
		info.Product = sysInfo.TypedSpec().ProductName
	}

	hostname, err := safe.StateGetByID[*network.HostnameStatus](ctx, c.COSI, network.HostnameID)
	if err != nil && !state.IsNotFoundError(err) {
		return nil, fmt.Errorf("error getting hostname: %w", err)
	}

	if hostname != nil {
		info.Hostname = hostname.TypedSpec().Hostname
	}

	links, err := safe.StateListAll[*network.LinkStatus](ctx, c.COSI)
	if err != nil {
		return nil, fmt.Errorf("error listing links: %w", err)
	}

	for link := range links.All() {
		spec := link.TypedSpec()

		if !spec.Physical() {
			continue
		}

		info.Links = append(info.Links, linkInfo{
			Name: link.Metadata().ID(),
			MAC:  spec.HardwareAddr.String(),
			Up:   spec.LinkState,
		})
	}

	neighbors, err := waitForLLDPNeighbors(ctx, c, lldpWait, logger)
	if err != nil {
		return nil, err
	}

	for i := range info.Links {
		info.Links[i].Neighbors = neighbors[info.Links[i].Name]
	}

	return info, nil
}

func waitForLLDPNeighbors(ctx context.Context, c *client.Client, lldpWait time.Duration, logger *zap.Logger) (map[string][]network.LLDPNeighborSpec, error) {
	const pollInterval = 5 * time.Second

	deadline := time.Now().Add(lldpWait)

	for {
		neighbors, err := listLLDPNeighbors(ctx, c)
		if err != nil {
			// LLDPNeighborStatus resources are available since Talos 1.15, older nodes can still be matched by other identifiers
			logger.Warn("LLDP neighbors are not available", zap.Error(err))

			return map[string][]network.LLDPNeighborSpec{}, nil
		}

		if len(neighbors) > 0 || !time.Now().Before(deadline) {
			return neighbors, nil
		}

		logger.Info("waiting for LLDP neighbors", zap.Duration("remaining", time.Until(deadline).Round(time.Second)))

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func listLLDPNeighbors(ctx context.Context, c *client.Client) (map[string][]network.LLDPNeighborSpec, error) {
	statuses, err := safe.StateListAll[*network.LLDPNeighborStatus](ctx, c.COSI)
	if err != nil {
		return nil, err
	}

	neighbors := map[string][]network.LLDPNeighborSpec{}

	for status := range statuses.All() {
		if len(status.TypedSpec().Neighbors) > 0 {
			neighbors[status.Metadata().ID()] = status.TypedSpec().Neighbors
		}
	}

	return neighbors, nil
}
