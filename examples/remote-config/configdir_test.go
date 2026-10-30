// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestLookup(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "common.yaml"), "common: true")
	writeFile(t, filepath.Join(dir, "default.yaml"), "default: true")
	writeFile(t, filepath.Join(dir, "u", "550E8400-E29B-41D4-A716-446655440000.yaml"), "uuid: true")
	writeFile(t, filepath.Join(dir, "s", "sn123", "10-base.yaml"), "serial: base")
	writeFile(t, filepath.Join(dir, "s", "sn123", "20-apps.yml"), "serial: apps")
	writeFile(t, filepath.Join(dir, "s", "sn123", "notes.txt"), "ignored")
	writeFile(t, filepath.Join(dir, "h", "server01.yaml"), "hostname: true")
	writeFile(t, filepath.Join(dir, "m", "aa-bb-cc.yaml"), "mac: vendor")
	writeFile(t, filepath.Join(dir, "m", "aa-bb-cc-11-22-33.yaml"), "mac: exact")
	writeFile(t, filepath.Join(dir, "lldp", "leaf-1", "ethernet1-1.yaml"), "lldp: leaf-1 port 1")
	writeFile(t, filepath.Join(dir, "lldp", "leaf-2.yaml"), "lldp: leaf-2 any port")
	writeFile(t, filepath.Join(dir, "lldp", "00-11-22-33-44-55", "ge-0-0-1.yaml"), "lldp: by chassis")

	store, err := newConfigStore(dir, "", "")
	require.NoError(t, err)

	for _, test := range []struct {
		name     string
		expected string
		info     nodeInfo
		matched  int
	}{
		{
			name:     "default",
			info:     nodeInfo{UUID: "unknown"},
			expected: "common: true\n---\ndefault: true\n",
			matched:  2,
		},
		{
			name:     "uuid case insensitive",
			info:     nodeInfo{UUID: "550e8400-e29b-41d4-a716-446655440000"},
			expected: "common: true\n---\nuuid: true\n",
			matched:  2,
		},
		{
			name:     "serial directory",
			info:     nodeInfo{Serial: "SN123"},
			expected: "common: true\n---\nserial: base\n---\nserial: apps\n",
			matched:  2,
		},
		{
			name:     "hostname",
			info:     nodeInfo{Hostname: "server01"},
			expected: "common: true\n---\nhostname: true\n",
			matched:  2,
		},
		{
			name: "mac exact and vendor prefix",
			info: nodeInfo{Links: []linkInfo{
				{Name: "eth0", MAC: "aa:bb:cc:11:22:33"},
				{Name: "eth1", MAC: "aa:bb:cc:44:55:66"},
				{Name: "eth2", MAC: "ff:ff:ff:ff:ff:ff"},
			}},
			expected: "common: true\n---\nmac: exact\n---\nmac: vendor\n",
			matched:  3,
		},
		{
			name: "lldp switch and port",
			info: nodeInfo{Links: []linkInfo{
				{Name: "eth0", Neighbors: []network.LLDPNeighborSpec{{SystemName: "leaf-1", ChassisID: "aa:aa:aa:aa:aa:aa", PortID: "Ethernet1/1"}}},
				{Name: "eth1", Neighbors: []network.LLDPNeighborSpec{{SystemName: "leaf-1", ChassisID: "aa:aa:aa:aa:aa:aa", PortID: "Ethernet1/2"}}},
			}},
			expected: "common: true\n---\nlldp: leaf-1 port 1\n",
			matched:  2,
		},
		{
			name: "lldp whole switch",
			info: nodeInfo{Links: []linkInfo{
				{Name: "eth0", Neighbors: []network.LLDPNeighborSpec{{SystemName: "leaf-2", PortID: "Ethernet1/7"}}},
			}},
			expected: "common: true\n---\nlldp: leaf-2 any port\n",
			matched:  2,
		},
		{
			name: "lldp chassis id fallback",
			info: nodeInfo{Links: []linkInfo{
				{Name: "eth0", Neighbors: []network.LLDPNeighborSpec{{ChassisID: "00:11:22:33:44:55", PortID: "ge-0/0/1"}}},
			}},
			expected: "common: true\n---\nlldp: by chassis\n",
			matched:  2,
		},
		{
			name: "same file matched once",
			info: nodeInfo{
				UUID:  "550e8400-e29b-41d4-a716-446655440000",
				Links: []linkInfo{{Name: "eth0", MAC: "aa:bb:cc:00:00:01"}, {Name: "eth1", MAC: "aa:bb:cc:00:00:02"}},
			},
			expected: "common: true\n---\nmac: vendor\n---\nuuid: true\n",
			matched:  3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := store.Lookup(&test.info)
			require.NoError(t, err)
			require.Equal(t, test.expected, string(result.Data))
			require.Len(t, result.Matched, test.matched)
		})
	}
}

func TestLookupNothing(t *testing.T) {
	store, err := newConfigStore(t.TempDir(), "", "")
	require.NoError(t, err)

	result, err := store.Lookup(&nodeInfo{UUID: "unknown"})
	require.NoError(t, err)
	require.Empty(t, result.Data)
	require.Empty(t, result.Matched)
}

func TestFinalizeConfig(t *testing.T) {
	base := "apiVersion: v1alpha1\nkind: HostnameConfig\nhostname: node-1\n"

	t.Run("invalid", func(t *testing.T) {
		_, err := finalizeConfig([]byte("apiVersion: v1alpha1\nkind: NoSuchKind\n"), provisionerOptions{})
		require.Error(t, err)
	})

	t.Run("unchanged without join API URL", func(t *testing.T) {
		data, err := finalizeConfig([]byte(base), provisionerOptions{})
		require.NoError(t, err)
		require.Equal(t, base, string(data))
	})

	t.Run("appends join configuration", func(t *testing.T) {
		data, err := finalizeConfig([]byte(base), provisionerOptions{
			JoinAPIURL:    "grpc://172.20.0.1:4000?jointoken=secret",
			ServerAddress: serverAddress(t),
			SinkPort:      "8080",
			LogPort:       "4001",
		})
		require.NoError(t, err)
		require.Contains(t, string(data), "kind: SideroLinkConfig")
		require.Contains(t, string(data), "apiUrl: grpc://172.20.0.1:4000?jointoken=secret")
		require.Contains(t, string(data), "kind: EventSinkConfig")
		require.Contains(t, string(data), "endpoint: '[fdae:41e4:649b:9303::1]:8080'")
		require.Contains(t, string(data), "kind: KmsgLogConfig")
		require.Contains(t, string(data), "url: tcp://[fdae:41e4:649b:9303::1]:4001")

		// the result must still be a valid machine configuration
		_, err = finalizeConfig(data, provisionerOptions{})
		require.NoError(t, err)
	})

	t.Run("keeps existing SideroLinkConfig", func(t *testing.T) {
		existing := base + "---\napiVersion: v1alpha1\nkind: SideroLinkConfig\napiUrl: https://example.com/\n"

		data, err := finalizeConfig([]byte(existing), provisionerOptions{JoinAPIURL: "grpc://172.20.0.1:4000"})
		require.NoError(t, err)
		require.Equal(t, existing, string(data))
	})
}

func serverAddress(t *testing.T) netip.Addr {
	t.Helper()

	addr, err := netip.ParseAddr("fdae:41e4:649b:9303::1")
	require.NoError(t, err)

	return addr
}
