# Install and host commands

`parcon`, run as root on a standalone Proxmox VE 9 node, installs and manages the whole project. `parcon install`
checks the host, creates the worker network and its firewall, creates a small controller VM, imports the runner
template, and registers the scale set. After that, `parcon status`, `parcon config`, `parcon update`, and
`parcon uninstall` are how the user runs it; the controller VM needs no login, since `parcon` reaches into it
through the guest agent. [`install/install.sh`](../install/install.sh) is only the bootstrap: it
downloads the release's `parcon`, checks it against the checksum written into it, and runs `parcon install`.

`parcon` changes the host only where the project needs it, almost entirely through Proxmox's own tools, and it can
remove everything it created. It installs only from a signed release.

## Goals

- **One download, one command.** No clone and no build tools. The one package to install first is Proxmox's own
  prerequisite for SDN DHCP, `dnsmasq`, and the one setting to turn on first is the datacenter firewall: `parcon`
  checks both and says how, but leaves them to the user.
- **Headless.** `parcon` runs in a root shell with no browser. The only browser step, creating the GitHub App,
  happens on any other device, and the user copies one code back.
- **Minimal host footprint.** The host gets `parcon` itself, its settings, Proxmox objects (pools, a role, a user
  and token, ACLs, VMs, an SDN zone, VNet, and subnet, and the worker firewall's IP set, security group, and one host
  rule), and host tuning only where the runners need it: on a node that is
  itself a VM, a udev rule that turns off the LAN NIC's offloads (see *Host NIC offloads*) and, on KVM, the
  `no-kvm-apf` kernel parameter (see *Host KVM async page faults*). No apt packages, no services of its own, no
  reboots, no
  cloud-init snippets, and no edits to files Proxmox or the user manages, such as `/etc/network/interfaces` or
  `storage.cfg`, except `/etc/kernel/cmdline` on a systemd-boot node, which has no room for a file of ours.
- **Independent of the LAN.** Workers never share a network with the host or the LAN. The host routes their network
  to the internet with SNAT, and the Proxmox firewall, applied to each worker's NIC, keeps them off the LAN and the
  host, whatever the LAN or VLAN setup is.
- **No-touch VMs.** Everything the user does, they do with `parcon` on the host. The controller VM keeps the secrets,
  which never leave it; the host keeps the settings, which hold none.
- **Safe to re-run.** A second `parcon install` continues an install that stopped, and `parcon update` continues an
  update that stopped. `parcon update --force` runs every step of an update again, to repair what a run that was
  killed partway or anything else left broken. `--dry-run` prints the plan and changes nothing.
- **Signed updates.** `parcon update` installs only a release whose checksums carry a signature from a key built
  into `parcon`.
- **Reversible.** `parcon uninstall` removes every object `parcon` or the controller created, found by tag and name,
  and `parcon` itself.

## Usage

Download, read, then run the bootstrap:

```bash
curl -fsSLO https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh
less install.sh
bash install.sh
```

Or run it in one line:

```bash
bash -c "$(curl -fsSL https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh)"
```

`install.sh` passes its arguments to `parcon install`. Every setting has a default, so `parcon` asks only for
confirmation and for the GitHub App step (see *GitHub App setup*), where you choose the organization or repository on
GitHub. `parcon install -h` lists the settings:

```bash
bash install.sh --storage local-zfs --set runners.max=4 --set worker.memory=16GiB
```

| Flag | Default | Setting |
| ---- | ------- | ------- |
| `--bridge` | `vmbr0` | LAN bridge for the controller VM |
| `--vlan` | none | VLAN tag on the LAN bridge |
| `--pve-address` | the host's address on the bridge | the address the controller reaches the API on |
| `--controller-ip` | `dhcp` | the controller VM's LAN address: `dhcp` or a CIDR such as `192.0.2.10/24` |
| `--lan-gateway` | none | the LAN's router, needed with a static address |
| `--worker-subnet` | `10.251.0.0/22` | the worker network; its DHCP range must hold `runners.max` plus 4 |
| `--storage` | `local-lvm` | storage for VM disks |
| `--vmid-range` | `10000-10099` | VMIDs for workers and templates |
| `--scale-set` | `proxmox-ubuntu-26.04` | the scale set's name, used in `runs-on` |
| `--labels` | the scale set's name | comma-separated `runs-on` labels |
| `--runner-group` | `default` | the GitHub runner group; repositories must use `default` |
| `--min-runners` | `0` | idle workers to keep booted |
| `--github-url` | where you install the App | `https://github.com/<org>` or `https://github.com/<owner>/<repo>` |
| `--set KEY=VALUE` | | any config key (see *Host settings*), repeatable |
| `--app manual --client-id ID` | `--app manifest` | use an existing GitHub App instead of creating one |
| `--dry-run`, `--yes` | | print the plan and stop; answer yes to every question |

The settings are saved on the host when the install starts, so a re-run continues with them and refuses new ones.
Each release's `install.sh` installs exactly its own release; on a node with an older install, it updates it.

After the install, `parcon` in `/usr/local/bin` does the rest:

| Command | Effect |
| ------- | ------ |
| `parcon status [--json]` | The state of the whole install; exits 1 if anything failed |
| `parcon config get <key>`, `get --all` | A setting's value, or every setting with its default and what it is |
| `parcon config set <key> <value>` | Change a setting and apply it: the controller restarts with it |
| `parcon config describe [<key>]` | Which values a setting takes, and when a change applies |
| `parcon config apply` | Push the settings to the controller again, for example after the API's pinned certificate was renewed |
| `parcon update [--pre] [--force] [--yes] [--dry-run]` | Update to the newest release, or pre-release with `--pre`. `--force` runs every step, even those already done for the release |
| `parcon check` | Every check below, and after an install, the controller's own checks |
| `parcon check network` | The worker network check from step 13, on demand |
| `parcon check config\|proxmox\|github\|template` | One of the controller's checks, run in the controller VM |
| `parcon uninstall [--yes] [--dry-run]` | Remove everything, `parcon` included |

## Architecture

```
 Proxmox VE 9 node (host: parcon, its settings, Proxmox objects, and the datacenter firewall)
 ┌──────────────────────────────────────────────────────────────────────────────────┐
 │  pool par-system                            pool par-runners                     │
 │  ┌──────────────────────┐   PVE API +       ┌─────────────────────────────────┐  │
 │  │ controller VM        │   guest agent     │ runner template (versioned)     │  │
 │  │  config + secrets    ├──────────────────►│   │ linked clone                │  │
 │  └──────────┬───────────┘                   │   ▼                             │  │
 │             │                               │ worker VMs (one job each), each │  │
 │             │                               │ with firewall=1: in dropped,    │  │
 │             │                               │ out through group par-worker    │  │
 │             │                               └───────────────┬─────────────────┘  │
 │             │                                               │ VNet parnet,       │
 │             │                                               │ subnet 10.251.0/22 │
 │             │           host: 10.251.0.1, SDN DHCP          │                    │
 │             │           (dnsmasq@parzone), SNAT  ◄──────────┘                    │
 │             │                      │                                             │
 │  ───────────┴──────────────────────┴──── LAN bridge (e.g. vmbr0)                 │
 └─────────────────────────────┬────────────────────────────────────────────────────┘
                               ▼
                  LAN ──► Proxmox API, GitHub, internet
```

- The **controller VM** runs the controller as a systemd service and holds the config and secrets. It sits on the
  LAN because it needs the Proxmox API and GitHub. It has no NIC on the worker network and reaches workers only
  through the Proxmox API and the guest agent.
- The **worker network** is an SDN simple zone `parzone` with SDN DHCP, one VNet `parnet`, and one subnet. The host
  has the subnet's first address, which is the workers' gateway, and SNATs their traffic out of its own uplink. SDN's
  dnsmasq (`dnsmasq@parzone`) hands out the rest of the subnet, with the host's DNS server.
- The **worker firewall** is Proxmox's own, applied at each worker's NIC in the host's bridge, where a job can't turn
  it off. Each worker's firewall is on, drops everything inbound that the worker didn't start, allows DHCP, and uses
  the datacenter security group `par-worker`: DNS to the host's resolver, then nothing on the worker subnet (SDN's
  IP set `+sdn/parnet-all`) and nothing in the IP set `par-blocked` (RFC 1918, CGNAT, link-local, the LAN, every
  address of the host, and the controller VM). The rest of the internet is allowed. It takes effect only with the datacenter firewall on, which the user turns on.
- The API token can act only on the `par-runners` pool. The controller VM lives in `par-system`, so a compromised
  controller can't change or delete itself, or any VM outside the runner pool. Nor can it change the firewall: the
  token has no privileges on the datacenter firewall, and the controller starts no worker whose firewall isn't on.
- **Workers** attach only to `parnet`.
- **`parcon` on the host** drives Proxmox through `pvesh`, `pveum`, and `qm` as root, and the controller VM through
  `qm guest exec`. It holds the settings but no secrets: the token's secret and the App's key go straight into the
  controller VM on a command's stdin.

## Release assets

Each release publishes the following files, built by `.github/workflows/release.yml` when release-please's release
pull request is merged (see *Commits and releases* in [AGENTS.md](../AGENTS.md)). Versions follow semver, chosen by
release-please from the Conventional Commits since the last release. Each image is boot-tested before it is
published, and the release stays a draft until every file is uploaded.

| Asset | Contents | Built by |
| ----- | -------- | -------- |
| `install.sh` | The bootstrap, with this release's version and `parcon`'s checksum written in | Release workflow |
| `parcon-<ver>-linux-amd64` | `parcon`, for the host and the controller VM | Release workflow |
| `parcon-<ver>.qcow2` | The controller VM: Ubuntu 26.04, `qemu-guest-agent`, `unattended-upgrades`, the `parcon` binary, user, and unit | Packer `qemu` builder in CI (`images/controller/`) |
| `par-runner-<ver>.qcow2` | The runner template: Ubuntu 26.04, `qemu-guest-agent`, the `runner` user, a pinned `actions/runner`, Docker, and a few basics | Packer `qemu` builder in CI (`images/runner/`) |
| `par-runner-<ver>.json` | The runner image's build manifest, including its `actions/runner` version | Packer `qemu` builder in CI |
| `SHA256SUMS` | Checksums of all of the above | Release workflow |
| `SHA256SUMS.sig` | An Ed25519 signature of `SHA256SUMS` by the release signing key | Release workflow |

The chain of trust:

1. `install.sh` holds the version and `parcon`'s SHA-256 (its `@PAR_VERSION@` and `@PAR_PARCON_SHA256@`
   placeholders, filled by `install/fill-release.sh`), and runs a downloaded `parcon` only if it matches. A copy from
   the source tree refuses to run.
2. `parcon` carries the public release signing keys (`internal/release/keys`). It accepts a release only if one of
   them verifies its `SHA256SUMS.sig`, then checks every asset it downloads against `SHA256SUMS`. `parcon update`
   does the same from the running `parcon`, so an update needs neither a new `install.sh` nor trust in anything but
   the key.
3. The release workflow signs with a key only its `publish` job can read (the `release` environment admits only
   `main`), and checks the signature against those public keys before it publishes (see *Release signing* in
   [AGENTS.md](../AGENTS.md)).

A version with a suffix, such as `0.2.0-rc.1`, is published as a pre-release: its `install.sh` installs it and
`parcon update --pre` updates to it, but the `releases/latest` links in *Usage* and a plain `parcon update` keep to
the latest full release.

**Why prebuilt images instead of stock Ubuntu cloud images:** stock images don't include `qemu-guest-agent`, and the
only way to add it at first boot is custom cloud-init user data. Proxmox stores that as snippet files, which means
enabling the `snippets` content type on a host storage and writing files to the host. Shipping images that already
include the agent avoids both changes to the host.

## Preflight checks

`parcon install` runs these first, and `parcon check` runs them any time. A failed **hard** check stops the install. A
failed **warn** check is shown in the plan and needs confirmation (or `--yes`). Before an install, `parcon check`
checks the default settings; `parcon install --dry-run` with your flags checks yours. After an install, it checks the
saved settings, skips the free space an install needs, and also runs the controller's checks in its VM.

| Check | How | Level |
| ----- | --- | ----- |
| Running as root | the effective user ID is 0 (`pveum` and `qm` need `root@pam`) | hard |
| Proxmox VE 9.x | `pveversion` reports `pve-manager/9.*` | hard |
| Running on a PVE node, not in a container | `pveversion` present, `/etc/pve` mounted, `systemd-detect-virt --container` false. A node that is itself a VM is fine | hard |
| KVM available | `/dev/kvm` exists | hard |
| Required tools present | `qm`, `pveum`, `pvesh`, `pvesm`, and `ip` (all ship with PVE). The bootstrap checks for amd64, the only build of `parcon` | hard |
| Standalone node | `/etc/pve/corosync.conf` doesn't exist, so the node isn't in a cluster. Clusters aren't supported (see the README's *Limitations*) | hard |
| Clock synchronized | `timedatectl show -p NTPSynchronized` is `yes` (GitHub App JWTs fail when clocks drift) | hard |
| Outbound HTTPS | `github.com`, `api.github.com`, and the release asset hosts (`objects.githubusercontent.com`, `release-assets.githubusercontent.com`) | hard |
| Target storage | exists, active, and accepts `images` content | hard |
| Linked-clone support | storage type is `lvmthin`, `zfspool`, `rbd`, or file-based with qcow2. Otherwise `linkedClone: false` | warn |
| Free space | 50 GiB free on the VM storage: the controller and template disks, and room for workers | hard |
| Download space | 6 GiB free in `/var/tmp`, on the host's root filesystem, where the images are downloaded before they are imported | hard |
| Free memory and CPU | host RAM and threads against `maxRunners` × worker size plus existing VMs | warn |
| LAN bridge | the bridge for the controller VM exists. VLAN tag valid if set | hard |
| LAN NIC offloads | on a node that is itself a VM (a virtio NIC under the LAN bridge), its offloads are off, now and at boot. `parcon install` and `parcon update` turn them off; `parcon check` warns until they are | warn |
| KVM async page faults | on a node that is a VM on KVM (`systemd-detect-virt --vm` prints `kvm`), the boot loader boots it with `no-kvm-apf`, and it booted with it. `parcon install` and `parcon update` set it; `parcon check` warns until they have and the node has rebooted | warn |
| API certificate | how the controller will verify it: the node's CA, the system CAs, or a pinned fingerprint for a certificate from a CA the host doesn't trust | warn if pinned |
| SDN available | `ifupdown2` installed and `/etc/network/interfaces` sources `/etc/network/interfaces.d/*`, so applying SDN works | hard |
| dnsmasq for SDN DHCP | the `dnsmasq` package is installed and its own `dnsmasq.service` is off, as Proxmox's SDN docs ask (`apt install dnsmasq`, then `systemctl disable --now dnsmasq`). `parcon` installs no packages, so it says how | hard |
| Datacenter firewall on | `/cluster/firewall/options` has `enable: 1`. Without it, no guest's firewall applies and workers would reach the LAN. Turning it on also filters the host's own traffic, so `parcon` leaves it to the user, who checks first that its rules let them in | hard |
| DNS server for workers | the node has an IPv4 DNS server that isn't the node itself (System > DNS). SDN DHCP hands it to workers, and the firewall lets DNS through to it, even on the LAN | hard |
| Worker subnet free | the worker subnet doesn't overlap any route or address on the host, other than our own VNet's, or the controller's static LAN subnet | hard |
| SDN and firewall names free | zone `parzone`, VNet `parnet`, security group `par-worker`, and IP set `par-blocked` are unused, or ours from an earlier run | hard |
| No name or ID clash | pools, user, role, and VMIDs are unused, or ours from an earlier run | hard |
| Pending SDN changes | no SDN zone or VNet other than ours has pending changes, since applying ours would apply theirs too. Uninstall doesn't apply while any exist, and says so | hard |

## Controller VM contract

The controller image, `parcon` on the host, and the controller agree on this layout:

- **User:** a system user `parcon` with no login shell, created by the controller image. `parcon run` runs as
  `parcon` from a systemd unit.
- **Binary:** `/usr/local/bin/parcon`, baked into the controller image. `parcon update` has the VM download the new
  release's binary and check it against the verified `SHA256SUMS`, replaces the file, and restarts the unit.
- **Config and secrets:** the directory `/etc/proxmox-actions-runners` is owned by `parcon:parcon` with mode `0700`.
  Every file in it is owned by `parcon` with mode `0600`: `config.yaml`, `pve-token`, each GitHub App's key as
  `github-app-<Client ID>.pem`, and, when the API serves the node's own certificate, `pve-ca.pem`. Releases before
  several Apps kept the one key as `github-app.pem`; `parcon update` copies it to its new name, and removes it once the
  controller runs with the config that names the new one.
- **Commands:** `parcon` on the host runs every `parcon` command in the VM as `parcon`, with
  `runuser -u parcon -- parcon …` through `qm guest exec`, so files that `parcon` writes get the right owner.
- **Before the App exists:** `github.apps`, and a target's `clientId` and `installationId`, may be left out of
  `config.yaml`, because step 11 checks Proxmox before step 12 creates the App. `parcon check config`,
  `parcon check proxmox`, and `parcon check template` work without them. `parcon run` and `parcon check github` fail
  with "the GitHub App isn't set up yet" until every target has its App and installation. The controller still
  reads the format before several targets (`github.configUrl` and `github.app`), as one App and one target, so a
  new `parcon` works with the config an older release rendered.
- **Who writes the config:** only `parcon` on the host, which renders it from the host settings (see *Host
  settings*) and rewrites it whenever they change. It writes `config.yaml.new`, has the VM's own
  `parcon check config -config config.yaml.new` accept it, and only then moves it into place, so a config that
  version can't read never replaces a working one. In the VM, `parcon` prints the non-secret values it learns (Client
  ID, App ID, slug, installation ID) and writes secrets only to their own files; it never writes YAML.
- **Status:** the running controller writes a non-secret report to `/run/parcon/status.json` after each pass
  (`RuntimeDirectory=parcon` in the unit): each scale set's session, desired and actual workers, and GitHub's last
  job counts. `parcon status` reads it through the guest agent. The controller never reads it back.

## Host settings

`/etc/proxmox-actions-runners/settings.yaml` on the host (root, `0600`) is the source of truth for the install:

| Section | What | Written by |
| ------- | ---- | ---------- |
| `network` | the bridge, VLAN, API address, the VMs' LAN addresses and router, the worker subnet | install flags |
| `proxmox` | the storage and the VMID range | install flags |
| `github` | `apps`: each App's Client ID, slug (for its install link), and owner; `targets`: each organization or repository, and the App and installation that reach it | the install and `parcon target add`, as they learn them |
| `scaleSet` | the name, labels, runner group, the most and fewest workers, and how long a worker may live | install flags, `runners.max`, `worker.maxLifetime` |
| `worker` | worker hardware; a field left out uses the GitHub-matching default | `worker.cores`, `worker.memory` |

Version 2 of the file, from releases before several targets, had one `github.configUrl` and one `github.app`. `parcon`
reads it as one App and one target, and `parcon update` saves it as version 3. A target that is waiting for its App or
installation isn't rendered into the controller's config, so the controller serves only the targets that are set up.

It holds nothing secret. `parcon` renders the controller's `config.yaml` from it together with what it reads from
the node each time (the node's name, the API's address and certificate, and whether the storage can make linked
clones), and checks the result with the controller's own config validation before it pushes it. The defaults live
only in `internal/config`, like every other default.

The **config keys** are the settings `parcon config` changes. Each has a description, the values it takes on this
node (for example, no more vCPUs than the node has), a default, and says when a change applies:

| Key | Values | Default |
| --- | ------ | ------- |
| `runners.max` | 1 to the number of worker IDs in the VMID range, and at least `minRunners` | 1 |
| `worker.cores` | 1 to the node's CPU threads | 2 |
| `worker.maxLifetime` | 10m to 5d (GitHub's job limit for self-hosted runners), such as `12h`, `1h30m`, or `2d` | 6h |
| `worker.memory` | 1GiB to the node's memory, in GiB or MiB, such as `8GiB` or `12288MiB` | 8GiB |

`parcon config set` refuses a value outside them with the key's description, the same text `parcon config describe`
prints, and exits 2. It warns, but allows, a combination the node likely can't run, such as more worker memory in
total than the node has. For a value it accepts, it:

1. refuses if the controller runs a different `parcon` than the host (run `parcon update` first);
2. pushes the new config the safe way described in the *Controller VM contract*;
3. saves the settings;
4. restarts `parcon.service` and waits for its scale set session. Running workers keep their jobs: a restarted
   controller adopts them. New workers get the new hardware.

`worker.maxLifetime` is the exception to "running workers keep their jobs": the controller checks every worker's age
against the current limit on every pass, so raising it gives running jobs more time at once, and lowering it destroys
any worker already older than the new limit, even in the middle of its job. A job that runs longer than 6 hours also
needs `timeout-minutes` in its workflow: GitHub cancels a job after 360 minutes unless the workflow allows more,
whatever runs it.

Adding a key means adding it to the registry in `internal/settings/keys.go`.

## Install flow

1. **Preflight.** Run the checks above.
2. **Collect settings** from the flags, or the defaults: scale set name and limits, storage, the LAN bridge and VLAN,
   the controller's LAN IP (DHCP or static), the worker subnet (default `10.251.0.0/22`, changeable if it clashes),
   and the VMID range. An earlier run's saved settings take their place.
3. **Show the plan.** List every object to be created or changed on the host, then ask for confirmation.
4. **`parcon` on the host:** copy the running `parcon` to `/usr/local/bin/parcon` and save the settings.
5. **Proxmox access objects** (`pveum`):
   - pools `par-system` and `par-runners`
   - role `PARController` with only the privileges in `internal/proxmox/access.go`, listed in the README
   - user `par@pve` and a privilege-separated token `par@pve!controller`
   - ACLs for that token on `/pool/par-runners`, the target storage, and `/sdn/zones/parzone/parnet`
   `parcon` holds the token secret only in memory and pipes it into the controller VM. It never reaches the host's
   disk, a command line, or the output.
6. **Worker network** (`pvesh`): create the simple zone `parzone` with SDN DHCP (`--dhcp dnsmasq --ipam pve`), the
   VNet `parnet`, and its subnet: the host at the first address as the gateway, SNAT on, a DHCP range of every other
   usable address, and the host's first IPv4 DNS server as the DNS server it hands out. Then apply the SDN config
   (`pvesh set /cluster/sdn`). See *Networking*.
   **Worker firewall** (`pvesh`): create the datacenter IP set `par-blocked` and the security group `par-worker`
   (see *Networking*), and the host rule that lets DHCP in on `parnet`, in case the host firewall is on. Each
   carries the comment `proxmox-actions-runners`.
7. **Host NIC offloads**, only on a node that is itself a VM: see *Host NIC offloads*. Then **KVM async page
   faults**, only on a node that is a VM on KVM: see *Host KVM async page faults*.
8. **Verify the release**: download `SHA256SUMS` and `SHA256SUMS.sig` and check the signature. Each image is then
   downloaded when a step needs it, into a temporary directory under `/var/tmp` that is removed afterward, and checked
   against `SHA256SUMS`.
9. **Import the runner template** in `par-runners` from `par-runner-<ver>.qcow2` into the first free VMID of the
   reserved IDs at the end of the range (`qm create` + `qm set --scsi0 <storage>:0,import-from=<file>`), with a
   cloud-init drive, the guest agent enabled, and `net0` on `parnet` with `firewall=1`. Give it the worker firewall
   (options `enable`, `dhcp`, `policy_in DROP`, `policy_out ACCEPT`, and the rule `GROUP par-worker`), which every
   clone copies. Tag it `par-managed`, `par-template`, `par-tv-<import time in Unix seconds>`,
   `par-rv-<actions/runner version>` (from the image's manifest), and `par-release-<ver>`, and convert it to a
   template. See *Runner image*.
10. **Create the controller VM** in `par-system` from `parcon-<ver>.qcow2`: 2 vCPU, 2 GiB RAM, a 6 GiB disk (the
    image's size). Use Proxmox's built-in cloud-init drive for hostname and network only (no user data, no
    snippets), tag it `par-managed,par-controller`, and start it. Once it has an address, add it to `par-blocked`.
11. **Configure the controller** through the guest agent once it has finished booting. Secrets go through
    `qm guest exec --pass-stdin`, so they never appear on a command line or on the host's disk:
    - `/etc/proxmox-actions-runners/config.yaml`, rendered from the settings, with no `github.app` yet (see
      *Controller VM contract*)
    - the Proxmox token in `/etc/proxmox-actions-runners/pve-token`
    - with Proxmox's own certificate, a copy of the node's CA (`/etc/pve/pve-root-ca.pem`) in
      `/etc/proxmox-actions-runners/pve-ca.pem`
    The controller connects to the API by IP and verifies its certificate against a DNS name the certificate lists
    (`tlsServerName`), so renewals don't break it. It verifies Proxmox's own certificate against the node's CA
    (`caCertFile`), and a custom or ACME certificate the host's system CAs trust against the controller's system CAs.
    Only a certificate from a CA the host doesn't trust is pinned by fingerprint (`tlsFingerprint`), which preflight
    warns about: after it is renewed, `parcon config apply` pins the new one. `parcon` then runs
    `parcon check proxmox` in the VM as `parcon`. It confirms the Proxmox VE version, that the token has every
    privilege it needs on the pool, storage, and VNet, and that the storage accepts VM disks.
12. **Create the GitHub App** with the manifest flow described under *GitHub App setup*. The code the user pastes is
    passed to `parcon github app create` in the controller VM, so the App's private key goes straight from GitHub
    into the controller VM and never passes through the host. `parcon` then waits for the user to install the App
    (`parcon github app wait-installation`), learns from the installation which organization or repository the
    runners serve, and saves it and the installation ID in the settings and the controller's config. It saves the
    Client ID as soon as the key is in the VM, so a re-run after a failure keeps waiting for the same App rather than
    creating another. It then runs `parcon check github` in the VM, which confirms that the App credentials produce
    an installation token and can reach the org or repo, and enables and starts `parcon.service`.
13. **Worker network check** (also `parcon check network`). `parcon` clones one worker from the template into a
    reserved VMID, tagged `par-managed,par-build` so the reconcile loop leaves it alone, waits for it to finish
    booting, and confirms through `qm guest exec` that the worker got a DHCP lease, reaches GitHub, and can't reach
    the Proxmox API or the controller VM. It then destroys the clone, and waits for the controller to log
    `scale set session opened`, which it does once the scale set is registered and its listener session is open. A
    re-run first destroys a clone a failed run left.
14. **Finish.** Tag the controller VM `par-release-<ver>`, which marks a finished install, and print the `runs-on:`
    label, the controller VM's ID and IP, and the `parcon` commands.

If any step fails, `parcon` stops and says why; fix the cause and run `parcon install` again to continue from there.
Each step checks for existing objects before it creates anything.

## Controller image

The controller image starts from the same pinned Ubuntu 26.04 cloud image as the runner image, turns on
`unattended-upgrades`, and includes `qemu-guest-agent`, which is how `parcon` configures it. Its disk is 6 GiB, the
VM's size. A built image holds about 2.5 GiB and grows only by logs and OS updates, but a kernel update from
`unattended-upgrades` briefly needs room for two kernels and a new initramfs, which 4 GiB can't hold.

The controller image follows the *Controller VM contract*. On top of it:

- `parcon` is statically linked and baked into the image at `/usr/local/bin/parcon`.
- The image creates the `parcon` user and `/etc/proxmox-actions-runners`, and installs `parcon.service`
  ([`deploy/parcon.service`](../deploy/parcon.service)) without enabling it; `parcon install` enables it in step 12.
  The unit starts only once `/etc/proxmox-actions-runners/config.yaml` exists.
- The image doesn't enforce the files' modes: `parcon` writes each one as `parcon` with `umask 077`, for example
  `runuser -u parcon -- sh -c 'umask 077; cat > FILE'`.

## GitHub App setup

GitHub Apps can only be created in a browser: there is no API for it, and the OAuth device flow (where you type a
code shown by a CLI) only works for an App that already exists. `parcon install` therefore uses GitHub's
[manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) with
a static helper page, and the user copies one line back:

1. `parcon` picks a random `state`, 32 bytes from the kernel's random source in unpadded base64url (43 characters, 256
   bits, so nobody can guess it), and prints a link to the helper page with it:
   `https://klponce.github.io/proxmox-actions-runners/app/v1/?state=<state>`. The user opens it on any device with a
   browser. The page refuses a link without a state of that shape.
2. The page asks one question, where the runners should work:
   - **An organization**, by name: its runners serve all of its repositories. The page posts to
     `https://github.com/organizations/<org>/settings/apps/new?state=<state>`, and the App asks for
     **Self-hosted runners: write** on the organization.
   - **My personal account**: GitHub allows self-hosted runners on a personal account only per repository, so the
     App asks for **Administration: write** on repositories, and the user picks the repository when installing it.
     The page posts to `https://github.com/settings/apps/new?state=<state>`, which creates the App in the signed-in
     account.

   The manifest also sets a name, `par-runners-<8 random hex digits>` (App names are unique across GitHub), `url`
   and an inactive webhook (`hook_attributes: {url: <project repository>, active: false}`), because the controller
   long-polls, no events, `redirect_url` back to the same page, and `public: false`. The page shows what it will
   send under *What is sent to GitHub*.
3. The user reviews the App on GitHub and clicks **Create GitHub App**. GitHub redirects back to the page with `code`
   and `state`. The page shows one line to copy, `<state>.<code>`, removes the code from the address bar, and warns
   that the line unlocks the App's private key for up to an hour.
4. The user pastes the line into `parcon`, which splits it at the first `.`, checks that the state is
   the one it printed, and pipes **only the code** into `parcon github app create` in the controller VM.
5. `parcon` prints the App's install link (`https://github.com/apps/<slug>/installations/new`) as the last line
   while it waits, so it is what the user sees. It leads to GitHub's own install page, where the user installs the
   App on the organization, or on the repository for a personal account. A private App can only be installed on the
   account that owns it, so the App and its installation can't disagree.
   `parcon github app wait-installation` polls until the installation appears. No second copy-back is needed.
6. `parcon` takes the runners' target from the installation: the organization, or the one repository the App can
   reach on a personal account. If it can reach several, `--github-url` picks one, or `parcon` asks. With
   `--github-url` set, the installation must match it. `parcon` saves the target and the installation ID in the
   settings, renders `configUrl`, `clientId`, `installationId`, and `privateKeyFile` into `github` in `config.yaml`,
   then runs `parcon check github`.

The `parcon` commands, run in the controller VM as `parcon`. Secrets come only on stdin, never as arguments, and are
never logged or printed:

| Command | Stdin | Stdout |
| ------- | ----- | ------ |
| `parcon github app create [-key-file PATH]` | the manifest code | `{"clientId":"…","appId":…,"slug":"…","owner":"…","ownerType":"Organization","keyFile":"…"}` |
| `parcon github app import -client-id ID [-key-file PATH]` | an existing App's PEM private key | nothing |
| `parcon github app wait-installation -client-id ID [-account LOGIN [-repository NAME]] [-key-file PATH] [-timeout 15m]` | nothing | `{"installationId":…,"account":"…","accountType":"User","repositories":["…"]}` |

- `-key-file` defaults to `/etc/proxmox-actions-runners/github-app-<Client ID>.pem`, so each App's key has its own
  file; `create` learns the Client ID from GitHub, and `import` and `wait-installation` from `-client-id`. `create` and
  `import` write it with mode
  `0600` through a temporary file in the same directory and a rename, so a reader never sees half a key. `create`
  creates the temporary file before it exchanges the code, so a key path it can't write fails without spending the
  single-use code.
- `create` calls `POST /app-manifests/{code}/conversions` and keeps only the App ID, slug, Client ID, owner, and
  private key. It drops the client secret and webhook secret, which the controller doesn't use. An invalid, used, or
  expired code fails with a message that says so.
- `import` is the `--app manual --client-id ID` path for an App that already exists: `parcon install` reads its
  private key from the terminal and pipes the key to `import`. `import` checks that it is a PEM-encoded RSA private key.
- `wait-installation` authenticates as the App with a JWT and polls `GET /app/installations`, every page of it,
  every 5 seconds until the App is installed. With `-account` it waits for the installation on that account, compared
  without regard to case; without it, it takes the App's only installation and fails if there are several, because
  an existing App may be public and installed by anyone. `parcon install` passes `-account` whenever it knows the
  owner: from `--github-url`, or from the App the manifest flow just created. On a personal account it also lists
  every repository the installation can reach (`GET /installation/repositories` with an installation token), since
  the runners need one of them; an organization's runners serve the whole organization. `-repository` keeps waiting
  until the installation reaches that repository, so it can be added on GitHub while parcon waits. Errors don't end
  the wait, because a network blip, a GitHub error, or a newly created App that GitHub doesn't know yet all pass. It
  prints each new error as it happens and fails after `-timeout` with the last one.

About the helper page:

- It lives in `site/app/v1/index.html` and is published with GitHub Pages by `.github/workflows/pages.yml`. It is
  plain HTML and JavaScript with no third-party scripts, analytics, or cookies. Its Content-Security-Policy allows
  no requests except the form `POST` to GitHub.
- Its path is versioned (`/app/v1/`). Once a release has shipped, a change to the manifest or the query parameters
  gets a new path, so older releases keep working.
- The code on the page can be exchanged for the App's private key by anyone who has it, until it is used or expires.
  The page says so, and `parcon` exchanges it at once and never logs it.

Typing a code from `parcon` into the page (the device-flow style) isn't possible: the page is static, so it has no
way to send the result back to `parcon`. That would need a hosted relay service, which would also see
the code that unlocks the private key.

## Metrics endpoint

*Deferred past v0.1.* Neither image includes nginx yet, and `parcon` doesn't serve metrics yet. The design:

`parcon` serves `/metrics`, `/healthz`, and `/readyz` as plain HTTP on `127.0.0.1:9465` only. nginx (from Ubuntu
main, so `unattended-upgrades` patches it) runs in the controller VM and serves them over HTTPS on the LAN at
`https://<controller-ip>:9464/`.

- The certificate is self-signed (ECDSA P-256) with the VM's hostname and LAN addresses as subject alternative
  names. It is generated inside the controller VM on first boot, never baked into the image, since every install
  would then share the same key. A timer regenerates it 30 days before it expires, which is 2 years after it was
  issued.
- `parcon status` prints the certificate's SHA-256 fingerprint so Prometheus can be pointed at it with `ca_file`, or
  at least checked by hand.
- There is no authentication for now. The endpoint exposes counts, latencies, and scale set names, but no secrets.
  Workers can't reach it because the worker firewall blocks the LAN.
- `/readyz` fails while the GitHub message session or the Proxmox API is unreachable. `/healthz` only checks that the
  process is running.

## Runner image

The runner template is imported, never built on the node. CI builds `par-runner-<ver>.qcow2` with Packer from
`images/runner/`: it starts from the pinned Ubuntu 26.04 cloud image, installs the runner and the one-job units,
runs the checks in `images/runner/tests/`, and cleans up the machine-id, SSH host keys, cloud-init state, and logs.
The image's disk size is the baseline that each worker's `freeDiskGiB` is added to, so it is kept small.

Templates are immutable. `parcon update` imports each new release's runner image as a new template with a newer `par-tv` tag,
and the controller clones the newest one. Linked clones depend on their template, so the controller deletes an old
template only once no worker references it (`par-tpl-<VMID>` on each worker). A `par-build` smoke-test clone doesn't
say which template it came from, so the controller deletes no template while one exists. `parcon` destroys its
clones itself, and `parcon uninstall` removes any that remain.

GitHub stops accepting a runner with auto-update disabled 30 days after a newer `actions/runner` release. Each
runner release therefore ships as a project release with a new runner image, and `parcon update` imports it. A
daily workflow (`.github/workflows/runner-release.yml`) opens a pull request that bumps the image's pin as soon as a
new `actions/runner` release is out.
The controller logs a warning when the template has been behind the latest release for 7 days and an error after
21, and `parcon status` and `parcon check template` report the same.

## Networking

There is one networking mode. Workers sit on a dedicated network that the host routes to the internet, and the
Proxmox firewall at each worker's NIC decides where their traffic may go:

| Piece | Where | Role |
| ----- | ----- | ---- |
| SDN zone `parzone`, VNet `parnet`, subnet (default `10.251.0.0/22`) | Proxmox host | The host has the subnet's first address, the workers' gateway, and SNATs their traffic out of its uplink |
| SDN DHCP (`dnsmasq@parzone`) | Proxmox host | Hands out the rest of the subnet, with the host's first IPv4 DNS server. IPAM (`pve`) keeps each worker's lease, freed when the worker is destroyed |
| Worker firewall | Each worker's NIC (`firewall=1`), in the host's bridge | On; inbound policy DROP, so nothing reaches a worker that it didn't start; DHCP allowed; outbound through the security group `par-worker` |
| Security group `par-worker` | Datacenter firewall | Accept DNS (UDP and TCP 53) to the host's DNS server, then drop `+sdn/parnet-all` and `+dc/par-blocked`. The rest of the internet is allowed by the outbound policy |
| IP set `parnet-all` | SDN scope, generated by Proxmox | The worker VNet's subnets: other workers and the host's address on it. Proxmox keeps it current |
| IP set `par-blocked` | Datacenter firewall | RFC 1918, CGNAT (`100.64.0.0/10`), and link-local ranges, the LAN bridge's networks, every address the host has outside the VNet, and the controller VM |
| Host rule | Host firewall | Accept DHCP (UDP 67) in on `parnet`, so SDN's dnsmasq gets workers' requests when the host firewall is on |

The user does two things first, which `parcon` checks and explains but doesn't do: install `dnsmasq` (and turn off
its own service), which Proxmox's SDN DHCP needs, and turn on the datacenter firewall, without which no guest's
firewall applies. Turning that firewall on also filters the host's own inbound traffic, by default allowing only
SSH and the web UI from the host's own network, so only the user can judge whether it locks anyone out.

Workers use DHCP (`ipconfig0: ip=dhcp` on the built-in cloud-init drive), so the controller keeps no address
state. The subnet must hold `runners.max` workers plus the reserved IDs; settings validation checks it.

Every worker's firewall comes from its template, which `parcon` gives it before converting it, and every clone
copies. The controller checks it before it registers a runner or boots a worker (on, inbound DROP, group
`proxmox.firewallGroup`), and destroys a worker without it, so a template imported by hand can't put an open worker
on the network. A firewall that is on for a VM still does nothing while the datacenter firewall is off, which the
controller's token can't see: `parcon status` and `parcon check` fail then, and `parcon update` refuses to run.

The firewall filters each worker's traffic where it leaves the worker's NIC, before the host routes it, so a job
with root in its VM can't change it. Because the group drops the worker subnet, workers can't connect to each
other. The group's references are scoped (`+sdn/`, `+dc/`, which Proxmox VE 8.3 added), so an IP set of the same
name in a guest's own firewall can't shadow them. They still share one layer-2 segment, so a hostile job could answer ARP for another worker's address; MAC
filtering (on by default) keeps it from sending from another MAC.

Why the Proxmox firewall rather than a gateway VM (releases before this design ran one, with dnsmasq and nftables, as
the workers' only way out), or attaching workers to an existing bridge or VLAN:

- One VM fewer to build, boot-test, release, patch, and replace on every update, and no single point of failure
  between the workers and the internet beyond the host itself.
- Isolation is enforced in the host's bridge, per worker, which also blocks traffic between workers.
- The cost: the host routes untrusted traffic, and isolation depends on the datacenter firewall staying on, which
  `parcon status` reports. Attaching workers to an existing bridge would make isolation depend on the user's
  switch, router, and VLAN setup instead, which `parcon` can't check at all.

`parcon status` shows whether the datacenter firewall is on, whether the subnet, IP set, group, and host rule match
what `parcon` sets up, and whether `dnsmasq@parzone` runs. `parcon check network` tests the network end to end from
a throwaway worker. `parcon update` corrects whatever drifted, such as the host's DNS server changing.

### Host NIC offloads

The node may itself be a VM, for example to keep the runners apart from a main Proxmox setup. Its NIC is then a
virtio NIC under the LAN bridge, and its offloads (GRO, GSO, TSO, and TX checksumming) are on by default. The NIC
merges incoming packets that the host then routes to the workers, and workers' downloads stall at a few KB/s while
the host's own run at full speed.

On such a node (a port of the LAN bridge whose driver is `virtio_net`), `parcon install` and `parcon update` turn
those offloads off with `ethtool -K`, and write `/etc/udev/rules.d/90-par-offloads.rules` so they stay off after a reboot. The rule
matches the NIC by the name Proxmox gives it (such as `nic0`) and sorts after `80-net-setup-link.rules`, which
renames it. This is a file of our own rather than a `post-up` line in `/etc/network/interfaces`, which Proxmox's GUI
rewrites. `parcon check` and `parcon status` warn while the offloads are on or the rule is missing, and
`parcon uninstall` removes the rule and turns the offloads back on. On a node that isn't a VM, none of this happens.

### Host KVM async page faults

On a node that is itself a VM on KVM, the outer host sends the node's kernel an async page fault when a page of the
node's memory isn't ready, because it is swapped out or being moved, so that the node can run something else
meanwhile. A Linux guest takes one only while a program runs, but the node runs VMs of its own, the workers, and the
outer host can deliver one while the node's kernel runs. The node then panics with "Host injected async #PF in
kernel mode", or a task hangs for good waiting for a page the outer host never reports ready. Plenty of free memory
on the outer host makes it rarer, not impossible.

The kernel parameter `no-kvm-apf` turns them off: the outer host then pauses the vCPU until the page is ready, as for
a guest without the feature. The rare wait costs microseconds to milliseconds, and the node keeps KVM's other
paravirtual features. The test is `systemd-detect-virt --vm` printing `kvm`: hardware KVM is required on every node,
and a node on another hypervisor never gets these faults.

`parcon install` and `parcon update` set it for the node's next boot, for each boot loader the node has:

- **GRUB** (`/etc/default/grub` exists): `/etc/default/grub.d/par-no-kvm-apf.cfg`, which appends it to
  `GRUB_CMDLINE_LINUX`, then `update-grub`. Proxmox keeps its own kernel pin in the same directory.
- **systemd-boot** (`/etc/kernel/cmdline` exists): it is appended to the file's first line, the one
  `proxmox-boot-tool` reads. The file has no room for a file of ours, so this is the one file of the user's that
  `parcon` edits.
- Then, when `proxmox-boot-tool` keeps ESPs in sync (`/etc/kernel/proxmox-boot-uuids`), `proxmox-boot-tool refresh`.

`parcon` never reboots the node: stopping it would kill running jobs. The plan and the end of install and update say
to reboot it, and `parcon check` and `parcon status` warn until the running kernel has `no-kvm-apf`. On a node with
neither boot loader, they warn and say to add it by hand. `parcon uninstall` removes the GRUB file and runs
`update-grub`, and takes `no-kvm-apf` out of `/etc/kernel/cmdline`, even if the user had put it there; async page
faults come back on at the next boot.

## Update and uninstall

Each VM `parcon` creates carries a `par-release-<version>` tag, and the controller VM gets its tag last, so it marks
a finished install or update. `parcon install` on a node with a finished install of an older release updates it; on
one with an unfinished install (the `par-system` pool exists), it continues it.

- **Update** (`parcon update`) is in place, run on the host:
  - It finds the newest release (with `--pre`, the newest pre-release too). If that is newer than the host's
    `parcon`, it shows the plan, verifies the release's signature, replaces `/usr/local/bin/parcon` with the
    release's, and runs it to do the rest, so each release's own code updates the node to it.
  - The release's runner image is imported as a new template, and the controller removes the old one once its
    workers are gone.
  - The worker network and firewall are compared with what this release sets up, and corrected where they differ.
  - An install from a release with a gateway VM can't be updated in place: `parcon update` refuses and says to run
    `parcon uninstall`, then `parcon install`. Its settings (version 1) are refused the same way.
  - The controller VM downloads the release's `parcon-<ver>-linux-amd64` itself and checks it against the checksum
    from the verified `SHA256SUMS`, which `parcon` passes it through the guest agent. The binary is far too big for
    the guest agent to carry, and config and secrets never leave the VM. `parcon` then pushes the config the new
    release renders, restarts `parcon.service`, and waits for its session. The VM's OS updates itself with
    `unattended-upgrades`.
  - On a node that is itself a VM, the LAN NIC's offloads are turned off if they aren't yet (see *Host NIC
    offloads*), and on KVM, `no-kvm-apf` is set for the next boot if it isn't yet (see *Host KVM async page faults*).
  - Each piece already at the release is left alone, so `parcon update` again continues an update that stopped. An
    install newer than the host's `parcon` is refused rather than downgraded.
  - `--force` runs every step, even one already done for the release, whether or not a newer release exists: it
    imports the runner template again (the controller removes the old one once unused), sets up the worker network
    and firewall again, reinstalls the controller's `parcon` and config, and runs the host tuning. It repairs what the
    release tags can't show, such as a template imported by a run that was killed partway. The safety checks stay: the release's signature, the plan and its question, the lock, and the
    controller's check of its new config. The controller VM itself must be running, since an update never replaces
    it; a broken one needs `parcon uninstall` and `parcon install`.
- **Uninstall** (`parcon uninstall`) first stops the controller and deletes the scale sets in GitHub, for every
  target (`parcon github scaleset delete` in the controller VM, which also unregisters their runners; with
  `-target URL` it deletes one target's, and a target GitHub refuses doesn't stop the others). It then destroys every
  VM tagged `par-managed` in the two pools (an older release's gateway VM too), clones before templates, removes the
  ACLs, token, user, role, and pools, the host rule, security group, and IP set, and the subnet, `parnet` VNet, and
  `parzone` zone, and applies the SDN config. The datacenter firewall stays on, and `dnsmasq` stays installed: both
  were the user's. On a node that is itself a VM, it
  removes the udev rule that keeps the LAN NIC's offloads off and turns them back on, and takes `no-kvm-apf` out of
  the kernel command line. Last, it removes the settings
  and `/usr/local/bin/parcon`. It doesn't touch anything it didn't create. The GitHub App stays; delete it in
  GitHub's settings if you no longer need it.

## Conventions

- The host commands live in `internal/installer`, and reach the host only through `pvecli` (commands) and `hostsys`
  (files and facts), so their tests run whole installs, updates, and uninstalls against a fake node.
- Every change to the host goes through `pvecli.Changer`, which logs it and honors `--dry-run`. Secrets are never
  arguments, so the log never holds one: they go to the controller VM on `qm guest exec`'s stdin. A test runs a whole
  install with sentinel secrets and checks every command line, output, error, and host file for them.
- Use only tools that ship with Proxmox VE 9, and parse their JSON output in Go.
- `install/install.sh` stays a bootstrap in Bash with `set -euo pipefail`. Its body is inside `main` and called on the
  last line, so a partial `curl | bash` download can't run half a script. It must pass `shellcheck`, and its bats
  tests (`install/tests`) stub `curl` and the checks it makes.
