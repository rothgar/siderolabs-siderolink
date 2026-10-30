// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/siderolink"
)

// configStore looks up machine configuration files for a node in the data directory.
//
// The layout mirrors github.com/siderolabs/talos-remote-config, with an additional "lldp" directory
// keyed by the switch and switch port the node is attached to:
//
//	data/
//	├── common.yaml                  # combined with every node's configuration
//	├── default.yaml                 # used when no identifier matches
//	├── u/<uuid>.yaml                # SMBIOS system UUID
//	├── s/<serial>.yaml              # SMBIOS serial number
//	├── h/<hostname>.yaml            # current hostname (e.g. from DHCP)
//	├── m/<mac>.yaml                 # MAC of a physical link, partial (prefix) matching
//	└── lldp/<switch>/<port>.yaml    # LLDP system name (or chassis ID) and port ID of the neighbor
//	    lldp/<switch>.yaml           # all ports of the switch
//
// Any of the files can be a directory holding several YAML files instead.
type configStore struct {
	dataDir       string
	commonConfig  string
	defaultConfig string
}

// lookupResult is the combined configuration and the files it was built from.
type lookupResult struct {
	Data    []byte
	Matched []string
}

func newConfigStore(dataDir, commonConfig, defaultConfig string) (*configStore, error) {
	st, err := os.Stat(dataDir)
	if err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}

	if !st.IsDir() {
		return nil, fmt.Errorf("data directory %q is not a directory", dataDir)
	}

	if commonConfig == "" {
		commonConfig = filepath.Join(dataDir, "common.yaml")
	}

	if defaultConfig == "" {
		defaultConfig = filepath.Join(dataDir, "default.yaml")
	}

	return &configStore{
		dataDir:       dataDir,
		commonConfig:  commonConfig,
		defaultConfig: defaultConfig,
	}, nil
}

// Lookup finds the configuration files matching the node and combines them into a single multi-document YAML.
func (s *configStore) Lookup(info *nodeInfo) (*lookupResult, error) {
	var paths []string

	for _, m := range []struct{ dir, value string }{
		{"u", info.UUID},
		{"s", info.Serial},
		{"h", info.Hostname},
	} {
		if path, ok := matchPath(filepath.Join(s.dataDir, m.dir), m.value); ok {
			paths = append(paths, path)
		}
	}

	for _, link := range info.Links {
		if path, ok := matchMAC(filepath.Join(s.dataDir, "m"), link.MAC); ok {
			paths = append(paths, path)
		}
	}

	for _, link := range info.Links {
		for _, neighbor := range link.Neighbors {
			if path, ok := matchLLDP(filepath.Join(s.dataDir, "lldp"), neighbor.SystemName, neighbor.ChassisID, neighbor.PortID); ok {
				paths = append(paths, path)
			}
		}
	}

	paths = slices.Compact(slices.Sorted(slices.Values(paths)))

	if len(paths) == 0 {
		if _, err := os.Stat(s.defaultConfig); err == nil {
			paths = append(paths, s.defaultConfig)
		}
	}

	if len(paths) == 0 {
		return &lookupResult{}, nil
	}

	if _, err := os.Stat(s.commonConfig); err == nil {
		paths = append([]string{s.commonConfig}, paths...)
	}

	var documents [][]byte

	for _, path := range paths {
		docs, err := readConfigFiles(path)
		if err != nil {
			return nil, err
		}

		documents = append(documents, docs...)
	}

	return &lookupResult{
		Data:    combineYAMLDocuments(documents),
		Matched: paths,
	}, nil
}

// matchLLDP looks up lldp/<switch>/<port> falling back to lldp/<switch>, trying the system name first and the chassis ID second.
func matchLLDP(dir, systemName, chassisID, portID string) (string, bool) {
	for _, switchName := range []string{systemName, chassisID} {
		switchPath, ok := matchPath(dir, switchName)
		if !ok {
			continue
		}

		st, err := os.Stat(switchPath)
		if err != nil {
			continue
		}

		if !st.IsDir() {
			return switchPath, true
		}

		if portPath, ok := matchPath(switchPath, portID); ok {
			return portPath, true
		}
	}

	return "", false
}

// matchMAC matches a MAC address from the most specific to the most general prefix:
// aa-bb-cc-11-22-33, aa-bb-cc-11-22, ..., aa.
func matchMAC(dir, mac string) (string, bool) {
	parts := strings.Split(normalizeValue(mac), "-")

	for i := len(parts); i > 0; i-- {
		if path, ok := matchPath(dir, strings.Join(parts[:i], "-")); ok {
			return path, true
		}
	}

	return "", false
}

// matchPath finds an entry in dir named after the normalized value: either a directory or a .yaml/.yml file.
func matchPath(dir, value string) (string, bool) {
	normalized := normalizeValue(value)
	if normalized == "" {
		return "", false
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}

	for _, entry := range entries {
		name := strings.ToLower(entry.Name())

		if name == normalized || name == normalized+".yaml" || name == normalized+".yml" {
			return filepath.Join(dir, entry.Name()), true
		}
	}

	return "", false
}

// normalizeValue turns an identifier into a filename: lowercase, with characters unsafe for filenames replaced by hyphens.
func normalizeValue(value string) string {
	return strings.ToLower(strings.NewReplacer(":", "-", "/", "-", "\\", "-", " ", "-").Replace(strings.TrimSpace(value)))
}

// readConfigFiles reads a YAML file, or all YAML files of a directory in name order.
func readConfigFiles(path string) ([][]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if !st.IsDir() {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}

		return [][]byte{data}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	var documents [][]byte

	for _, entry := range entries {
		ext := strings.ToLower(filepath.Ext(entry.Name()))

		if entry.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			return nil, err
		}

		documents = append(documents, data)
	}

	return documents, nil
}

// combineYAMLDocuments joins the documents with the YAML document separator.
func combineYAMLDocuments(documents [][]byte) []byte {
	var out bytes.Buffer

	for i, doc := range documents {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		if i > 0 {
			out.WriteString("\n---\n")
		}

		out.Write(doc)
	}

	if out.Len() > 0 {
		out.WriteByte('\n')
	}

	return out.Bytes()
}

// finalizeConfig validates that the combined YAML is a machine configuration and, if the join API URL is known,
// appends the join configuration so that the node keeps its SideroLink connection after the configuration is applied.
//
// Applying a configuration replaces the node's current one, including the join configuration it booted with.
func finalizeConfig(data []byte, opts provisionerOptions) ([]byte, error) {
	cfg, err := configloader.NewFromBytes(data)
	if err != nil {
		return nil, err
	}

	if opts.JoinAPIURL == "" || cfg.SideroLink() != nil {
		return data, nil
	}

	extra, err := joinConfigDocuments(opts)
	if err != nil {
		return nil, err
	}

	return combineYAMLDocuments([][]byte{data, extra}), nil
}

// joinConfigDocuments renders the "join configuration": the machine configuration documents which make a node
// connect to this server over SideroLink and send its events and kernel logs here.
func joinConfigDocuments(opts provisionerOptions) ([]byte, error) {
	apiURL, err := url.Parse(opts.JoinAPIURL)
	if err != nil {
		return nil, fmt.Errorf("invalid join API URL: %w", err)
	}

	serverAddr := opts.ServerAddress.String()

	siderolinkCfg := siderolink.NewConfigV1Alpha1()
	siderolinkCfg.APIUrlConfig = meta.URL{URL: apiURL}

	eventSinkCfg := runtime.NewEventSinkV1Alpha1()
	eventSinkCfg.Endpoint = net.JoinHostPort(serverAddr, opts.SinkPort)

	kmsgLogCfg := runtime.NewKmsgLogV1Alpha1()
	kmsgLogCfg.MetaName = "remote-config"
	kmsgLogCfg.KmsgLogURL = meta.URL{URL: &url.URL{Scheme: "tcp", Host: net.JoinHostPort(serverAddr, opts.LogPort)}}

	ctr, err := container.New([]config.Document{siderolinkCfg, eventSinkCfg, kmsgLogCfg}...)
	if err != nil {
		return nil, err
	}

	out, err := ctr.EncodeString(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return nil, err
	}

	return []byte(out), nil
}
