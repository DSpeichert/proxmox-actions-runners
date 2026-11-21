# Install and host commands

`parcon`, run as root on a standalone Proxmox VE 9 node, installs and manages the whole project. `parcon install`
checks the host, creates the worker network and its firewall, imports the runner template, sets up the controller as
`parcon.service` on the host, and registers the scale set. After that, `parcon status`, `parcon config`,
`parcon target`, `parcon update`, and `parcon uninstall` are how the user runs it. [`install/install.sh`](../install/install.sh) is only the bootstrap: it
downloads the release's `parcon`, checks it against the checksum written into it, and runs `parcon install`.

`parcon` changes the host only where the project needs it, almost entirely through Proxmox's own tools, and it can
remove everything it created. It installs only from a signed release.

## Goals

- **One download, one command.** No clone and no build tools. The one package to install first is Proxmox's own
  prerequisite for SDN DHCP, `dnsmasq`, and the one setting to turn on first is the datacenter firewall: `parcon`
  checks both and says how, but leaves them to the user.
- **Headless.** `parcon` runs in a root shell with no browser. The only browser step, creating the GitHub App,
  happens on any other device, and the user copies one code back.
- **Minimal host footprint.** The host gets `parcon` itself, its settings, the controller's service with its config
  and encrypted credentials (see *Controller service contract*), Proxmox objects (a pool, a role, a user and token,
  ACLs, VMs, an SDN zone, VNet, and subnet, and the worker firewall's IP set, security group, and one host rule), and
  host tuning only where the runners need it: on a node that is
  itself a VM, a udev rule that turns off the LAN NIC's offloads (see *Host NIC offloads*) and, on KVM, the
  `no-kvm-apf` kernel parameter (see *Host KVM async page faults*). No apt packages, one service of its own, no
  reboots, no
  cloud-init snippets, and no edits to files Proxmox or the user manages, such as `/etc/network/interfaces` or
  `storage.cfg`, except `/etc/kernel/cmdline` on a systemd-boot node, which has no room for a file of ours.
- **Independent of the LAN.** Workers never share a network with the host or the LAN. The host routes their network
  to the internet with SNAT, and the Proxmox firewall, applied to each worker's NIC, keeps them off the LAN and the
  host, whatever the LAN or VLAN setup is.
- **Secrets only for the controller.** Everything the user does, they do with `parcon` on the host. The controller's
  secrets are encrypted with `systemd-creds` the moment `parcon` gets them, and only the controller's sandbox sees
  them decrypted; the settings hold none.
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
| `--bridge` | `vmbr0` | the host's LAN bridge: the API's address, and the LAN workers are kept off |
| `--pve-address` | the host's address on the bridge | the address the controller reaches the API on |
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
| `parcon target list [--json]` | The organizations and repositories the runners serve, each with its App, installation, and session |
| `parcon target add --github-url URL [--app manual --client-id ID] [--yes] [--dry-run]` | Serve another organization or repository too (see *Several GitHub targets*) |
| `parcon target remove URL [--yes] [--dry-run]` | Stop serving one: delete its scale set in GitHub, then push the config without it, which destroys its workers |
| `parcon update [--pre] [--force] [--yes] [--dry-run]` | Update to the newest release, or pre-release with `--pre`. `--force` runs every step, even those already done for the release |
| `parcon check` | Every check below, and after an install, the controller's own checks |
| `parcon check network` | The worker network check from step 12, on demand |
| `parcon check config\|proxmox\|github\|template` | One of the controller's checks, run in its sandbox |
| `parcon uninstall [--yes] [--dry-run]` | Remove everything, `parcon` included; the GitHub Apps stay in GitHub |

## Architecture

```
 Proxmox VE 9 node (host: parcon, its settings, Proxmox objects, and the datacenter firewall)
 ┌──────────────────────────────────────────────────────────────────────────────────┐
 │  parcon.service (sandboxed,                 pool par-runners                     │
 │  DynamicUser)                               ┌─────────────────────────────────┐  │
 │  ┌──────────────────────┐   PVE API +       │ runner template (versioned)     │  │
 │  │ controller           │   guest agent     │   │ linked clone                │  │
 │  │  credentials         ├──────────────────►│   ▼                             │  │
 │  └──────────┬───────────┘                   │ worker VMs (one job each), each │  │
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
                  LAN ──► GitHub, internet
```

- The **controller** is `parcon run`, started by `parcon.service` on the host as a transient user in a systemd
  sandbox, with its config and secrets as systemd credentials (see *Controller service contract*). It talks HTTPS
  to GitHub and to the Proxmox API on the host's address, and reaches workers only through the API and the guest
  agent. It listens on no port.
- The **worker network** is an SDN simple zone `parzone` with SDN DHCP, one VNet `parnet`, and one subnet. The host
  has the subnet's first address, which is the workers' gateway, and SNATs their traffic out of its own uplink. SDN's
  dnsmasq (`dnsmasq@parzone`) hands out the rest of the subnet, with the host's DNS server.
- The **worker firewall** is Proxmox's own, applied at each worker's NIC in the host's bridge, where a job can't turn
  it off. Each worker's firewall is on, drops everything inbound that the worker didn't start, allows DHCP, and uses
  the datacenter security group `par-worker`: DNS to the host's resolver, then nothing on the worker subnet (SDN's
  IP set `+sdn/parnet-all`) and nothing in the IP set `par-blocked` (RFC 1918, CGNAT, link-local, the LAN, every
  address of the host, the controller's included). The rest of the internet is allowed. It takes effect only with the datacenter firewall on, which the user turns on.
- The API token can act only on the `par-runners` pool, so a compromised controller can't change or delete any VM
  outside the runner pool. Nor can it change the firewall: the token has no privileges on the datacenter firewall,
  and the controller starts no worker whose firewall isn't on. The sandbox keeps it from Proxmox's own secrets in
  `/etc/pve`, root's files, and the host's devices, but it shares the host's kernel: that is the price of running
  it on the host rather than in a VM of its own (see [design.md](design.md#managing-the-install-from-the-host)).
- **Workers** attach only to `parnet`.
- **`parcon` on the host** drives Proxmox through `pvesh`, `pveum`, and `qm` as root, and runs the controller's own
  commands in its sandbox with `systemd-run`. It keeps the settings, which hold no secrets: the token's secret and
  each App's key go straight to `systemd-creds encrypt` on its stdin.

## Release assets

Each release publishes the following files, built by `.github/workflows/release.yml` when release-please's release
pull request is merged (see *Commits and releases* in [AGENTS.md](../AGENTS.md)). Versions follow semver, chosen by
release-please from the Conventional Commits since the last release. Each image is boot-tested before it is
published, and the release stays a draft until every file is uploaded.

| Asset | Contents | Built by |
| ----- | -------- | -------- |
| `install.sh` | The bootstrap, with this release's version and `parcon`'s checksum written in | Release workflow |
| `parcon-<ver>-linux-amd64` | `parcon`: the host commands and the controller | Release workflow |
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
| Required tools present | `qm`, `pveum`, `pvesh`, `pvesm`, `ip`, `systemctl`, `systemd-run`, `systemd-creds`, and `journalctl` (all ship with PVE). The bootstrap checks for amd64, the only build of `parcon` | hard |
| Standalone node | `/etc/pve/corosync.conf` doesn't exist, so the node isn't in a cluster. Clusters aren't supported (see the README's *Limitations*) | hard |
| No controller VM from an older release | no VM tagged `par-controller` or `par-gateway` and no pool `par-system`: releases before the controller ran on the host can't be moved over, so the user runs that release's `parcon uninstall` first | hard |
| systemd credentials | systemd 254 or newer, for `ImportCredential=`. It reports whether `systemd-creds` will use a TPM2 (`systemd-creds has-tpm2`) or the host's key alone. It encrypts nothing: `systemd-creds` makes the host's key when the install first encrypts a secret | hard |
| Clock synchronized | `timedatectl show -p NTPSynchronized` is `yes` (GitHub App JWTs fail when clocks drift) | hard |
| Outbound HTTPS | `github.com`, `api.github.com`, and the release asset hosts (`objects.githubusercontent.com`, `release-assets.githubusercontent.com`) | hard |
| Target storage | exists, active, and accepts `images` content | hard |
| Linked-clone support | storage type is `lvmthin`, `zfspool`, `rbd`, or file-based with qcow2. Otherwise `linkedClone: false` | warn |
| Free space | 50 GiB free on the VM storage: the template's disk, and room for workers | hard |
| Download space | 4 GiB free in `/var/tmp`, on the host's root filesystem, where the runner image is downloaded before it is imported | hard |
| Free memory and CPU | host RAM and threads against `maxRunners` × worker size plus existing VMs and the controller's 512 MiB | warn |
| LAN bridge | the bridge exists, and the host has an address on it for the API, or `--pve-address` names one | hard |
| LAN NIC offloads | on a node that is itself a VM (a virtio NIC under the LAN bridge), its offloads are off, now and at boot. `parcon install` and `parcon update` turn them off; `parcon check` warns until they are | warn |
| KVM async page faults | on a node that is a VM on KVM (`systemd-detect-virt --vm` prints `kvm`), the boot loader boots it with `no-kvm-apf`, and it booted with it. `parcon install` and `parcon update` set it; `parcon check` warns until they have and the node has rebooted | warn |
| API certificate | how the controller will verify it: the node's CA, the system CAs, or a pinned fingerprint for a certificate from a CA the host doesn't trust | warn if pinned |
| SDN available | `ifupdown2` installed and `/etc/network/interfaces` sources `/etc/network/interfaces.d/*`, so applying SDN works | hard |
| dnsmasq for SDN DHCP | the `dnsmasq` package is installed and its own `dnsmasq.service` is off, as Proxmox's SDN docs ask (`apt install dnsmasq`, then `systemctl disable --now dnsmasq`). `parcon` installs no packages, so it says how | hard |
| Datacenter firewall on | `/cluster/firewall/options` has `enable: 1`. Without it, no guest's firewall applies and workers would reach the LAN. Turning it on also filters the host's own traffic, so `parcon` leaves it to the user, who checks first that its rules let them in | hard |
| DNS server for workers | the node has an IPv4 DNS server that isn't the node itself (System > DNS). SDN DHCP hands it to workers, and the firewall lets DNS through to it, even on the LAN | hard |
| Worker subnet free | the worker subnet doesn't overlap any route or address on the host, other than our own VNet's | hard |
| SDN and firewall names free | zone `parzone`, VNet `parnet`, security group `par-worker`, and IP set `par-blocked` are unused, or ours from an earlier run | hard |
| No name or ID clash | pools, user, role, and VMIDs are unused, or ours from an earlier run | hard |
| Pending SDN changes | no SDN zone or VNet other than ours has pending changes, since applying ours would apply theirs too. Uninstall doesn't apply while any exist, and says so | hard |

## Controller service contract

`parcon` on the host, the unit it writes, and the controller agree on this layout. The controller is the same
`parcon` binary as the host commands, so the two never disagree about the config's format.

- **Binary:** `/usr/local/bin/parcon`. `parcon update` replaces it, then restarts the unit.
- **Unit:** `/etc/systemd/system/parcon.service`, which `parcon` renders (`renderUnit` in `internal/installer`) and
  rewrites on install, `parcon config apply`, and update when it differs. It runs `parcon run` with
  `DynamicUser=yes`: a user that exists only while the service runs, owns no file outside the service's runtime
  directory, and has no capabilities. The sandbox (`sandboxProperties`) makes the system read-only, hides
  `/etc/pve`, `/var/lib/pve-cluster`, `/root`, parcon's own directory, the credential stores, and systemd's
  credential key, gives the service private `/tmp`, `/dev`, users, and IPC, filters its system calls, allows only
  IPv4, IPv6, and Unix sockets, and caps it at 512 MiB of memory and 256 tasks. `systemd-analyze security
  parcon.service` reports its exposure.
- **Config:** `/etc/proxmox-actions-runners/config.yaml` (root, `0600`), handed to the service as the credential
  `parcon.config`. When the API serves the node's own certificate, the node's CA is next to it as `pve-ca.pem`, handed
  over as `parcon.pve-ca.pem`. The config names its secrets as `${CREDENTIALS_DIRECTORY}/<credential>`, which the
  controller expands to the service's credentials directory.
- **Secrets:** each is a credential encrypted with `systemd-creds encrypt --with-key=auto --tpm2-pcrs=` in
  `/etc/credstore.encrypted` (root, `0600`): `parcon.pve-token`, the Proxmox API token's secret, and
  `parcon.github-app-<Client ID>`, each GitHub App's private key. The key is the host's
  (`/var/lib/systemd/credential.secret`), and the TPM2 too when the node has one; the credentials aren't bound to
  PCRs, so firmware and Secure Boot updates don't lock them. The unit imports them with `ImportCredential=parcon.*`.
  A secret reaches `systemd-creds` only on its stdin, never on a command line or the disk unencrypted. Copying the
  credentials to another host doesn't work: they need this host's key, and its TPM2 if they were made with one.
- **Commands:** the host runs the controller's own commands (`parcon check`, `parcon github app wait-installation`,
  `parcon github scaleset delete`) in the same sandbox, with the same credentials, through
  `systemd-run --pipe --wait`. `parcon` tells it runs there by `$CREDENTIALS_DIRECTORY/parcon.config`, and refuses
  those commands anywhere else.
- **Before the App exists:** `github.apps`, and a target's `clientId` and `installationId`, may be left out of
  `config.yaml`, because the install checks Proxmox before it creates the App. `parcon check config`,
  `parcon check proxmox`, and `parcon check template` work without them. `parcon run` and `parcon check github` fail
  with "the GitHub App isn't set up yet" until every target has its App and installation.
- **Who writes the config:** only `parcon` on the host, which renders it from the host settings (see *Host
  settings*), checks it the way the controller reads it, and rewrites it atomically whenever they change.
- **Status:** the running controller writes a non-secret report to `/run/parcon/status.json` after each pass
  (`RuntimeDirectory=parcon` in the unit): each scale set's session, desired and actual workers, and GitHub's last
  job counts. `parcon status` reads it. The controller never reads it back.

## Host settings

`/etc/proxmox-actions-runners/settings.yaml` on the host (root, `0600`) is the source of truth for the install:

| Section | What | Written by |
| ------- | ---- | ---------- |
| `network` | the bridge, the API address, the worker subnet | install flags |
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

1. renders and checks the controller's config, and writes it (see *Controller service contract*);
2. saves the settings;
3. restarts `parcon.service` and waits for its scale set session. Running workers keep their jobs: a restarted
   controller adopts them. New workers get the new hardware.

`worker.maxLifetime` is the exception to "running workers keep their jobs": the controller checks every worker's age
against the current limit on every pass, so raising it gives running jobs more time at once, and lowering it destroys
any worker already older than the new limit, even in the middle of its job. A job that runs longer than 6 hours also
needs `timeout-minutes` in its workflow: GitHub cancels a job after 360 minutes unless the workflow allows more,
whatever runs it.

Adding a key means adding it to the registry in `internal/settings/keys.go`.

## Install flow

1. **Preflight.** Run the checks above.
2. **Collect settings** from the flags, or the defaults: scale set name and limits, storage, the LAN bridge, the
   worker subnet (default `10.251.0.0/22`, changeable if it clashes), and the VMID range. An earlier run's saved
   settings take their place.
3. **Show the plan.** List every object to be created or changed on the host, then ask for confirmation.
4. **`parcon` on the host:** copy the running `parcon` to `/usr/local/bin/parcon` and save the settings.
5. **Proxmox access objects** (`pveum`):
   - pool `par-runners`, created first: its comment marks the install as ours
   - role `PARController` with only the privileges in `internal/proxmox/access.go`, listed in the README
   - user `par@pve`; its privilege-separated token `par@pve!controller`, and the ACLs for it on `/pool/par-runners`,
     the target storage, and `/sdn/zones/parzone/parnet`, come with the controller in step 10
6. **Worker network** (`pvesh`): create the simple zone `parzone` with SDN DHCP (`--dhcp dnsmasq --ipam pve`), the
   VNet `parnet`, and its subnet: the host at the first address as the gateway, SNAT on, a DHCP range of every other
   usable address, and the host's first IPv4 DNS server as the DNS server it hands out. Then apply the SDN config
   (`pvesh set /cluster/sdn`). See *Networking*.
   **Worker firewall** (`pvesh`): create the datacenter IP set `par-blocked` and the security group `par-worker`
   (see *Networking*), and the host rule that lets DHCP in on `parnet`, in case the host firewall is on. Each
   carries the comment `proxmox-actions-runners`.
7. **Host NIC offloads**, only on a node that is itself a VM: see *Host NIC offloads*. Then **KVM async page
   faults**, only on a node that is a VM on KVM: see *Host KVM async page faults*.
8. **Verify the release**: download `SHA256SUMS` and `SHA256SUMS.sig` and check the signature. The runner image is
   then downloaded into a temporary directory under `/var/tmp` that is removed afterward, and checked against
   `SHA256SUMS`.
9. **Import the runner template** in `par-runners` from `par-runner-<ver>.qcow2` into the first free VMID of the
   reserved IDs at the end of the range (`qm create` + `qm set --scsi0 <storage>:0,import-from=<file>`), with a
   cloud-init drive, the guest agent enabled, and `net0` on `parnet` with `firewall=1`. Give it the worker firewall
   (options `enable`, `dhcp`, `policy_in DROP`, `policy_out ACCEPT`, and the rule `GROUP par-worker`), which every
   clone copies. Tag it `par-managed`, `par-template`, `par-tv-<import time in Unix seconds>`,
   `par-rv-<actions/runner version>` (from the image's manifest), and `par-release-<ver>`, and convert it to a
   template. See *Runner image*.
10. **Configure the controller** (see *Controller service contract*):
    - `/etc/proxmox-actions-runners/config.yaml`, rendered from the settings, with no `github.apps` yet
    - with Proxmox's own certificate, a copy of the node's CA (`/etc/pve/pve-root-ca.pem`) in
      `/etc/proxmox-actions-runners/pve-ca.pem`
    - `/etc/systemd/system/parcon.service`, then `systemctl daemon-reload`
    - the token `par@pve!controller`: `parcon` holds its secret only in memory and pipes it into
      `systemd-creds encrypt`, which writes `/etc/credstore.encrypted/parcon.pve-token`. It never reaches the disk
      unencrypted, a command line, or the output. A token whose credential is missing is replaced.
    - the token's ACLs
    The controller connects to the API by IP and verifies its certificate against a DNS name the certificate lists
    (`tlsServerName`), so renewals don't break it. It verifies Proxmox's own certificate against the node's CA
    (`caCertFile`), and a custom or ACME certificate the host's system CAs trust against the system CAs. Only a
    certificate from a CA the host doesn't trust is pinned by fingerprint (`tlsFingerprint`), which preflight warns
    about: after it is renewed, `parcon config apply` pins the new one. `parcon` then runs `parcon check proxmox` as
    the controller, in its sandbox. It confirms the Proxmox VE version, that the token has every privilege it needs on
    the pool, storage, and VNet, and that the storage accepts VM disks.
11. **Create the GitHub App** with the manifest flow described under *GitHub App setup*. `parcon` spends the code the
    user pastes with GitHub itself and pipes the App's private key straight into `systemd-creds encrypt`
    (`/etc/credstore.encrypted/parcon.github-app-<Client ID>`); the key never reaches the disk unencrypted.
    `parcon` then waits for the user to install the App (`parcon github app wait-installation`, as the controller),
    learns from the installation which organization or repository the runners serve, and saves it and the
    installation ID in the settings and the controller's config. It saves the Client ID as soon as the key is
    encrypted, so a re-run after a failure keeps waiting for the same App rather than creating another. It then runs
    `parcon check github` as the controller, which confirms that the App credentials produce an installation token and
    can reach the org or repo, and enables and starts `parcon.service`.
12. **Worker network check** (also `parcon check network`). `parcon` clones one worker from the template into a
    reserved VMID, tagged `par-managed,par-build` so the reconcile loop leaves it alone, waits for it to finish
    booting, and confirms through `qm guest exec` that the worker got a DHCP lease, reaches GitHub, and can't reach
    the Proxmox API on the host. It then destroys the clone, and waits for the controller to log
    `scale set session opened`, which it does once the scale set is registered and its listener session is open. A
    re-run first destroys a clone a failed run left.
13. **Finish.** Record the release in the settings (`installed`), which marks a finished install, and print the
    `runs-on:` label and the `parcon` commands.

If any step fails, `parcon` stops and says why; fix the cause and run `parcon install` again to continue from there.
Each step checks for existing objects before it creates anything.

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
   the one it printed, and exchanges **only the code** with GitHub (`POST /app-manifests/{code}/conversions`). It keeps
   the App ID, slug, Client ID, owner, and private key, and drops the client secret and webhook secret, which the
   controller doesn't use. The key goes straight into `systemd-creds encrypt`. An invalid, used, or expired code
   fails with a message that says so.
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

With `--app manual --client-id ID`, for an App that already exists, `parcon install` reads its private key from the
terminal, checks that it is a PEM-encoded RSA private key, and encrypts it the same way.

The `parcon` command the host runs as the controller, in its sandbox, to wait for the installation:

| Command | Stdout |
| ------- | ------ |
| `parcon github app wait-installation -client-id ID [-account LOGIN [-repository NAME]] [-key-file PATH] [-timeout 15m]` | `{"installationId":…,"account":"…","accountType":"User","repositories":["…"]}` |

- `-key-file` defaults to the App's credential, `$CREDENTIALS_DIRECTORY/parcon.github-app-<Client ID>`.
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

### Several GitHub targets

`parcon install` sets up one target, an organization or a personal account's repository. `parcon target add
--github-url URL` adds more to a finished install, in steps that each check what already exists, so a re-run
continues:

1. **Plan.** It shows the target, the App it will use, and that `runners.max` is shared by every target, and asks.
   It needs the install finished at the host `parcon`'s release, since it writes a config that release renders.
2. **App.** A private App can be installed only on its owner, so there is one App per organization or personal
   account. An account that already has one reuses it: its own App, or the App of another of its targets. Otherwise
   the user creates one with the helper page, as at install, or `--app manual --client-id ID` imports an existing
   App's key. The key is encrypted for the controller as the credential `parcon.github-app-<Client ID>`. If the user picks another account
   on the helper page, the new App is kept for that account when it has none yet, or its key is removed when it
   has one, and `parcon` says what to do.
3. **Installation.** `wait-installation -account <owner>`, with `-repository <repo>` for a repository, waits until
   the App is installed there and reaches the repository. On a personal account whose App is installed already, the
   user only adds the repository to that installation. The target is saved as soon as it is added, and its App and
   installation once they are known; the controller serves it only from then on.
4. **Controller.** `parcon check github` checks every target, and the controller restarts with the new config. `parcon`
   waits until the controller's log shows a session for every target.

`parcon target remove URL` refuses the last target, which only `parcon uninstall` removes. It deletes the target's
scale sets first with `parcon github scaleset delete -target URL`, using the config that still names the target, so
if GitHub refuses, nothing else has changed. It then writes the config without the target and restarts the
controller. The controller destroys a worker whose target is no longer configured without asking GitHub, once the
worker isn't ready or has run as long as the runner check waits. The App's key goes when no other target uses the
App: its credential is removed. The App itself stays in GitHub.

## Metrics endpoint

*Deferred past v0.1.* `parcon` doesn't serve metrics yet. The design:

`parcon` serves `/metrics`, `/healthz`, and `/readyz` as plain HTTP on `127.0.0.1:9465` only, from its sandbox on the
host. How they are exposed to Prometheus beyond the host, if at all, is the user's choice: `parcon` installs no web
server or packages.

- There is no authentication for now. The endpoint exposes counts, latencies, and scale set names, but no secrets.
  Workers can't reach it because the worker firewall blocks the host.
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
| IP set `par-blocked` | Datacenter firewall | RFC 1918, CGNAT (`100.64.0.0/10`), and link-local ranges, the LAN bridge's networks, every address the host has outside the VNet, which covers the controller on it |
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

The settings record the release of a finished install or update (`installed`), which an install or update sets
last. `parcon install` on a node with a finished install of an older release updates it; on one with an unfinished
install (the `par-runners` pool exists, with parcon's comment), it continues it. Each runner template carries a
`par-release-<version>` tag of the release that imported it.

- **Update** (`parcon update`) is in place, run on the host:
  - It finds the newest release (with `--pre`, the newest pre-release too). If that is newer than the host's
    `parcon`, it shows the plan, verifies the release's signature, replaces `/usr/local/bin/parcon` with the
    release's, and runs it to do the rest, so each release's own code updates the node to it.
  - The release's runner image is imported as a new template, and the controller removes the old one once its
    workers are gone.
  - The worker network and firewall are compared with what this release sets up, and corrected where they differ.
  - An install from a release with a controller VM (or, before that, a gateway VM) can't be updated in place:
    `parcon update` refuses and says to run that release's `parcon uninstall`, then `parcon install`. Its settings
    (versions 1 to 3) are refused the same way.
  - The controller is the new `parcon` itself: `parcon` writes the config and unit the new release renders, restarts
    `parcon.service`, and waits for its sessions. Running workers keep going, and the restarted controller adopts
    them. With `--assets`, an update from local files works the same way.
  - On a node that is itself a VM, the LAN NIC's offloads are turned off if they aren't yet (see *Host NIC
    offloads*), and on KVM, `no-kvm-apf` is set for the next boot if it isn't yet (see *Host KVM async page faults*).
  - Each piece already at the release is left alone, so `parcon update` again continues an update that stopped. An
    install newer than the host's `parcon` is refused rather than downgraded.
  - `--force` runs every step, even one already done for the release, whether or not a newer release exists: it
    imports the runner template again (the controller removes the old one once unused), sets up the worker network
    and firewall again, rewrites the controller's config and unit and restarts it, and runs the host tuning. It
    repairs what the release tags can't show, such as a template imported by a run that was killed partway. The
    safety checks stay: the release's signature, the plan and its question, and the lock.
- **Uninstall** (`parcon uninstall`) first deletes the scale sets in GitHub, for every target
  (`parcon github scaleset delete` as the controller, which also unregisters their runners; with `-target URL` it
  deletes one target's, and a target GitHub refuses doesn't stop the others), then stops and disables
  `parcon.service` and removes its unit, its config and CA copy, and its credentials (`/etc/credstore.encrypted/parcon.*`;
  systemd's own key stays, since other services may use it). It then destroys every VM tagged `par-managed` in the
  runner pool, clones before templates, removes the ACLs, token, user, role, and pool, the host rule, security group,
  and IP set, and the subnet, `parnet` VNet, and `parzone` zone, and applies the SDN config. The datacenter firewall
  stays on, and `dnsmasq` stays installed: both were the user's. On a node that is itself a VM, it removes the udev
  rule that keeps the LAN NIC's offloads off and turns them back on, and takes `no-kvm-apf` out of the kernel command
  line. Last, it removes the settings and `/usr/local/bin/parcon`. It doesn't touch anything it didn't create. The
  GitHub Apps stay; delete them in GitHub's settings if you no longer need them.

## Conventions

- The host commands live in `internal/installer`, and reach the host only through `pvecli` (commands) and `hostsys`
  (files and facts), so their tests run whole installs, updates, and uninstalls against a fake node.
- Every change to the host goes through `pvecli.Changer`, which logs it and honors `--dry-run`. Secrets are never
  arguments, so the log never holds one: they go to `systemd-creds encrypt` on its stdin. A test runs a whole
  install with sentinel secrets and checks every command line, output, error, and host file for them.
- Use only tools that ship with Proxmox VE 9, and parse their JSON output in Go.
- `install/install.sh` stays a bootstrap in Bash with `set -euo pipefail`. Its body is inside `main` and called on the
  last line, so a partial `curl | bash` download can't run half a script. It must pass `shellcheck`, and its bats
  tests (`install/tests`) stub `curl` and the checks it makes.
