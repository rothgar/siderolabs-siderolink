# SideroLink remote config example

An example SideroLink server which configures Talos Linux nodes based on what it learns about them
over the SideroLink tunnel: hardware identifiers, network links and LLDP neighbors.

It is the SideroLink counterpart of [talos-remote-config](https://github.com/siderolabs/talos-remote-config).
With `talos-remote-config` the node fetches its configuration from an HTTP endpoint passed via the
`talos.config` kernel argument. Here the direction is reversed: the node boots with a small *join
configuration* which connects it to this server, and the server queries the node over the tunnel and
pushes the full configuration back.

## How it works

1. A node boots with a join configuration (a `SideroLinkConfig` document pointing at this server) and
   establishes a WireGuard tunnel to the server. The server assigns the node an address in the
   `fdae:41e4:649b:9303::/64` SideroLink network.
2. The join configuration is not a complete machine configuration, so the node stays in maintenance mode.
   In maintenance mode with SideroLink configured the Talos API listens only on the node's SideroLink
   address, and it accepts connections without client certificates.
3. The server is notified about the new peer (it implements `wireguard.PeerHandler`) and dials the
   node's Talos API over the tunnel.
4. It reads from the node:
   * the Talos version,
   * `SystemInformation`: system UUID, serial number, manufacturer and product,
   * `HostnameStatus`: the hostname the node currently has (from DHCP, for example),
   * `LinkStatus`: every physical link and its MAC address,
   * `LLDPNeighborStatus`: the switch (chassis ID, system name) and switch port (port ID, description,
     management addresses, VLANs) each physical link is plugged into. The server waits up to `-lldp-wait`
     for the first LLDP advertisement to arrive.
5. The node information is logged, and used to look up configuration files in the data directory
   (see below). The matching files are combined into one multi-document machine configuration.
6. The combined configuration is validated, the join configuration is appended to it if it lacks a
   `SideroLinkConfig` document (applying a configuration replaces the one the node booted with), and the
   result is applied with the Talos `ApplyConfiguration` API. The node installs itself and reboots.
7. When the node comes back it reconnects over SideroLink with its full configuration in place. Such
   nodes require client certificates on the Talos API, so the server logs that the node is already
   configured and leaves it alone.

The server also runs the event sink and the kernel log receiver from `siderolink-agent`: the events
and kernel logs the nodes send over the tunnel show up in its log.

## Running

The server needs to create a WireGuard interface, so it either runs as root or with
`-sidero-link-force-userspace`.

```bash
cd examples/remote-config
go build -o remote-config .

mkdir -p data

sudo ./remote-config \
  -sidero-link-join-token secret \
  -sidero-link-wireguard-endpoint 172.20.0.1:51821 \
  -join-api-url grpc://172.20.0.1:4000 \
  -join-config-file join-config.yaml \
  -data-dir ./data \
  -dry-run
```

`172.20.0.1` is the address of the server as the nodes see it. The default SideroLink API listens on
`:4000` without TLS, hence the `grpc://` scheme in the join API URL. With `-join-api-url` set the server
prints the join configuration on startup and writes it to `-join-config-file`:

```yaml
apiVersion: v1alpha1
kind: SideroLinkConfig
apiUrl: grpc://172.20.0.1:4000?jointoken=secret
---
apiVersion: v1alpha1
kind: EventSinkConfig
endpoint: '[fdae:41e4:649b:9303::1]:8080'
---
apiVersion: v1alpha1
kind: KmsgLogConfig
name: remote-config
url: tcp://[fdae:41e4:649b:9303::1]:4001
```

### Booting nodes with the join configuration

The join configuration is the same for every node, so it can be delivered in any of the usual ways:

* **Maintenance mode**: boot a node from a stock Talos image and apply the join configuration to it:

  ```bash
  talosctl apply-config --insecure -n <node IP> -f join-config.yaml
  ```

* **Kernel arguments**: compress the join configuration and pass it inline, for example when PXE
  booting with [booter](https://github.com/siderolabs/booter):

  ```bash
  INLINE=$(zstd --compress --ultra -22 < join-config.yaml | base64 -w 0)

  docker run --rm --network host ghcr.io/siderolabs/booter:v0.3.0 \
    --extra-kernel-args "talos.config.inline=${INLINE}"
  ```

  The equivalent of the join configuration as plain kernel arguments is
  `siderolink.api=grpc://172.20.0.1:4000?jointoken=secret talos.events.sink=[fdae:41e4:649b:9303::1]:8080 talos.logging.kernel=tcp://[fdae:41e4:649b:9303::1]:4001`.

* **ISO volume**: put the join configuration on a volume labelled `metal-iso` as `config.yaml` and
  boot with `talos.config=metal-iso`.

* **QEMU**: `talosctl cluster create qemu` boots nodes with only a `SideroLinkConfig` document when
  `--omni-api-endpoint` is given:

  ```bash
  talosctl cluster create qemu --omni-api-endpoint 'grpc://172.20.0.1:4000?jointoken=secret' ...
  ```

### Dry run first

Run with `-dry-run` first. For every node that connects the server logs what it found, for example:

```yaml
address: fdae:41e4:649b:9303:2a0f:4c6b:b3d1:7e42
talosVersion: v1.15.0
uuid: 550e8400-e29b-41d4-a716-446655440000
serial: SN12345678
manufacturer: Supermicro
product: SYS-5019C-M
hostname: talos-abc-123
links:
  - name: eth0
    mac: aa:bb:cc:11:22:33
    up: true
    neighbors:
      - chassisID: 00:11:22:33:44:55
        systemName: leaf-1
        systemDescription: Arista Networks EOS version 4.32.1F
        portID: Ethernet1/1
        portDescription: rack 7 server 3
        managementAddresses:
          - 10.0.0.1
        vlans:
          - id: 10
            name: servers
  - name: eth1
    mac: aa:bb:cc:11:22:34
    up: false
```

Use these values to name the configuration files, then restart the server without `-dry-run` and
reboot the node.

## Data directory

The layout follows `talos-remote-config`, with an additional `lldp` directory keyed by the switch
and switch port the node is attached to:

```text
data/
├── common.yaml                 # combined with every node's configuration
├── default.yaml                # used when no identifier matches
├── u/                          # system UUID:         u/550e8400-e29b-41d4-a716-446655440000.yaml
├── s/                          # serial number:       s/sn12345678.yaml
├── h/                          # hostname:            h/talos-abc-123.yaml
├── m/                          # MAC of a link:       m/aa-bb-cc-11-22-33.yaml (partial matching, see below)
└── lldp/                       # LLDP neighbor:
    ├── leaf-1/                 #   switch system name (or chassis ID) ...
    │   ├── ethernet1-1.yaml    #   ... and port ID
    │   └── ethernet1-2/        #   a directory of YAML files works as well
    │       ├── 10-base.yaml
    │       └── 20-apps.yaml
    └── leaf-2.yaml             #   all ports of a switch
```

Matching rules:

* Identifiers are lowercased and `:`, `/`, `\` and spaces are replaced with `-` to form the file name,
  so port `Ethernet1/1` on switch `leaf-1` matches `lldp/leaf-1/ethernet1-1.yaml` and chassis ID
  `00:11:22:33:44:55` matches `lldp/00-11-22-33-44-55/`.
* Each match can be a `.yaml`/`.yml` file or a directory, in which case all YAML files in it are used in
  name order.
* MAC addresses are matched from the most specific to the most general prefix: `aa-bb-cc-11-22-33`,
  `aa-bb-cc-11-22`, ..., `aa`. Every physical link of the node is looked up.
* LLDP neighbors are looked up by system name first, then by chassis ID. Every neighbor of every
  physical link is looked up.
* A file matched by several identifiers is used once. The matched files are combined in the order
  `common.yaml`, then the identifier matches sorted by path.
* If no identifier matches, `default.yaml` is used (if it exists). If there is nothing to apply, the node
  is left in maintenance mode and a warning is logged.

Files hold machine configuration documents: either a complete `v1alpha1` configuration with the
install section, or partial documents which together form one. All of them are joined with `---`.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-sidero-link-wireguard-endpoint` | `172.20.0.1:51821` | WireGuard endpoint advertised to the nodes |
| `-sidero-link-api-endpoint` | `:4000` | SideroLink gRPC API listen address |
| `-sidero-link-join-token` | | join token the nodes have to present |
| `-sidero-link-force-userspace` | `false` | use a userspace WireGuard device |
| `-event-sink-endpoint` | `:8080` | event sink listen address |
| `-log-receiver-endpoint` | `:4001` | kernel log receiver listen address |
| `-data-dir` | `./data` | directory with the configuration files |
| `-common-config` | `<data-dir>/common.yaml` | configuration combined with every node's configuration |
| `-default-config` | `<data-dir>/default.yaml` | configuration used when nothing matches |
| `-join-api-url` | | SideroLink API URL as reachable from the nodes; enables the join configuration |
| `-join-config-file` | | write the join configuration to this file on startup |
| `-apply-mode` | `auto` | `auto`, `no-reboot` or `staged` |
| `-dry-run` | `false` | log the configuration instead of applying it |
| `-connect-timeout` | `2m` | how long to wait for the node's API after it connects |
| `-lldp-wait` | `45s` | how long to wait for LLDP neighbors |
| `-debug` | `false` | debug logging |

## Notes

* LLDP neighbors are reported by Talos 1.15 and later (`LLDPNeighborStatus` resources). Talos receives
  LLDP passively on every physical link, the switch has to be configured to send it. Older nodes are
  still matched by UUID, serial, hostname and MAC.
* The SideroLink API is served without TLS by default (`grpc://`). To serve it over TLS, set
  `agent.Config.APITLSConfig` in `main.go` and use `https://` in the join API URL.
* Nodes which already have a full configuration are skipped. If an applied configuration does not
  include an install section, the node reboots back into maintenance mode and is configured again on
  the next connection, which is the same behaviour as `talos-remote-config`.
* This module is separate from the SideroLink Go module and uses a `replace` directive to build
  against the checkout it lives in.
