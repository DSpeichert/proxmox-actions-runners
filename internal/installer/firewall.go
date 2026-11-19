package installer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// The worker firewall's objects in the datacenter firewall. Uninstall finds them by these names, and the host's
// DHCP rule by its comment.
const (
	FirewallGroup = config.WorkerFirewallGroup
	BlockedIPSet  = config.BlockedIPSet
	// FirewallComment marks the firewall objects and the host rule as ours.
	FirewallComment = PoolComment
)

// alwaysBlocked are the ranges workers never reach, whatever the LAN looks like: RFC 1918, CGNAT, and link-local.
var alwaysBlocked = []string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"}

// workerNet is the worker subnet's layout: the host's address on the VNet, which is the workers' gateway, and the
// DHCP range, every other usable address.
type workerNet struct {
	subnet               netip.Prefix
	gateway, first, last netip.Addr
}

func workerNetwork(s *settings.Settings) (workerNet, error) {
	subnet, err := netip.ParsePrefix(s.Network.WorkerSubnet)
	if err != nil || !subnet.Addr().Is4() {
		return workerNet{}, fmt.Errorf("worker subnet %q isn't an IPv4 subnet", s.Network.WorkerSubnet)
	}
	subnet = subnet.Masked()
	base := subnet.Addr().As4()
	broadcast := binary.BigEndian.Uint32(base[:]) | (1<<(32-subnet.Bits()) - 1)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], broadcast)
	gateway := subnet.Addr().Next()
	return workerNet{subnet: subnet, gateway: gateway, first: gateway.Next(), last: netip.AddrFrom4(b).Prev()}, nil
}

// dhcpRange is the subnet's DHCP range in pvesh's format.
func (w workerNet) dhcpRange() string {
	return "start-address=" + w.first.String() + ",end-address=" + w.last.String()
}

// hostDNS returns the DNS server workers use: the node's first IPv4 DNS server that isn't on the node itself. SDN
// DHCP hands it out, and the worker firewall lets DNS through to it even when it is on the LAN.
func (in *Installer) hostDNS(ctx context.Context, s *settings.Settings) (netip.Addr, error) {
	node, err := in.nodeName(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	servers, err := in.PVE.DNS(ctx, node)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read the node's DNS servers: %w", err)
	}
	w, err := workerNetwork(s)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, srv := range servers {
		a, err := netip.ParseAddr(srv)
		if err != nil || !a.Is4() || a.IsLoopback() || a.IsUnspecified() || w.subnet.Contains(a) {
			continue
		}
		return a, nil
	}
	return netip.Addr{}, fmt.Errorf("the node has no IPv4 DNS server workers can use (it has %v); set one under "+
		"the node's System > DNS", servers)
}

// blocked returns what workers must not reach, beyond the worker subnet itself: the always-blocked ranges, the host's
// networks on the LAN bridge, every address the host has on any other interface (the API listens on all of them),
// and the controller VM once it has an address. The worker subnet, with the host's address on it, is SDN's own IP set
// for the VNet (vnetSubnets), which follows the subnet without parcon copying it.
func (in *Installer) blocked(ctx context.Context, s *settings.Settings, controller netip.Addr) ([]string, error) {
	addrs, err := in.Sys.Addrs(ctx)
	if err != nil {
		return nil, err
	}
	block := slices.Clone(alwaysBlocked)
	for _, a := range addrs {
		if a.Dev == VNet {
			continue
		}
		if a.Dev == s.Network.Bridge {
			block = append(block, a.Prefix.Masked().String())
		}
		if !a.Prefix.Addr().IsLoopback() {
			block = append(block, a.Prefix.Addr().String())
		}
	}
	if pve, err := in.pveAddress(ctx, s); err == nil {
		block = append(block, pve.String())
	}
	if controller.IsValid() {
		block = append(block, controller.String())
	}
	if p, err := netip.ParsePrefix(s.Network.ControllerIP); err == nil {
		block = append(block, p.Addr().String())
	}
	slices.Sort(block)
	return slices.Compact(block), nil
}

// vnetSubnets is SDN's IP set of the worker VNet's subnets (Proxmox VE 8.3 and later generate one for every VNet),
// in the firewall's scoped syntax: other workers and the host's address on the VNet.
const vnetSubnets = "+sdn/" + VNet + "-all"

// workerRules are the security group's rules, in order: DNS to the host's resolver, then nothing on the worker
// network and nothing in the blocked IP set. References are scoped (+sdn/, +dc/), so no IP set of the same name in a
// worker's own firewall can shadow them. The rest of the internet is allowed by the workers' outbound policy, and
// nothing comes in that they didn't start: their inbound policy drops it. DHCP is allowed by their firewall's dhcp
// option.
func workerRules(dns netip.Addr) []pvecli.Rule {
	return []pvecli.Rule{
		{Type: "out", Action: "ACCEPT", Proto: "udp", Dest: dns.String(), Dport: "53", Enable: 1},
		{Type: "out", Action: "ACCEPT", Proto: "tcp", Dest: dns.String(), Dport: "53", Enable: 1},
		{Type: "out", Action: "DROP", Dest: vnetSubnets, Enable: 1},
		{Type: "out", Action: "DROP", Dest: "+dc/" + BlockedIPSet, Enable: 1},
	}
}

// hostRule lets workers' DHCP requests through the host firewall to SDN's dnsmasq, in case the host firewall is on.
func hostRule() pvecli.Rule {
	return pvecli.Rule{Type: "in", Action: "ACCEPT", Iface: VNet, Proto: "udp", Dport: "67", Enable: 1,
		Comment: FirewallComment}
}

// sameRule compares what a rule does, not where it is or its comment.
func sameRule(a, b pvecli.Rule) bool {
	a.Pos, b.Pos, a.Comment, b.Comment = 0, 0, "", ""
	return a == b
}

func sameRules(a, b []pvecli.Rule) bool { return slices.EqualFunc(a, b, sameRule) }

// ruleArgs are pvesh's arguments for a rule.
func ruleArgs(r pvecli.Rule) []string {
	args := []string{"--type", r.Type, "--action", r.Action, "--enable", strconv.Itoa(r.Enable)}
	for _, kv := range [][2]string{{"--proto", r.Proto}, {"--dport", r.Dport}, {"--dest", r.Dest},
		{"--iface", r.Iface}, {"--comment", r.Comment}} {
		if kv[1] != "" {
			args = append(args, kv[0], kv[1])
		}
	}
	return args
}

// configureFirewall creates or corrects the worker firewall's objects: the blocked IP set, the security group, and
// the host's DHCP rule. Each change keeps workers at least as restricted as before it: entries are added to the IP
// set before stale ones go, and new group rules go in above the old ones before those are removed.
func (in *Installer) configureFirewall(ctx context.Context, s *settings.Settings, controller netip.Addr) error {
	in.Out.Step("Worker firewall")
	block, err := in.blocked(ctx, s, controller)
	if err != nil {
		return err
	}
	dns, err := in.hostDNS(ctx, s)
	if err != nil {
		return err
	}
	if err := in.syncIPSet(ctx, block); err != nil {
		return err
	}
	if err := in.syncGroup(ctx, workerRules(dns)); err != nil {
		return err
	}
	return in.syncHostRule(ctx)
}

func (in *Installer) syncIPSet(ctx context.Context, want []string) error {
	sets, err := in.PVE.IPSets(ctx)
	if err != nil {
		return err
	}
	var have []string
	if !slices.Contains(sets, BlockedIPSet) {
		if err := in.run(ctx, "pvesh", "create", "/cluster/firewall/ipset", "--name", BlockedIPSet, "--comment",
			FirewallComment+": what workers must not reach"); err != nil {
			return err
		}
	} else if have, err = in.PVE.IPSet(ctx, BlockedIPSet); err != nil {
		return err
	}
	for _, cidr := range want {
		if !slices.Contains(have, cidr) {
			if err := in.run(ctx, "pvesh", "create", "/cluster/firewall/ipset/"+BlockedIPSet, "--cidr", cidr); err != nil {
				return err
			}
		}
	}
	for _, cidr := range have {
		if !slices.Contains(want, cidr) {
			if err := in.run(ctx, "pvesh", "delete", "/cluster/firewall/ipset/"+BlockedIPSet+"/"+cidr); err != nil {
				return err
			}
		}
	}
	return nil
}

func (in *Installer) syncGroup(ctx context.Context, want []pvecli.Rule) error {
	groups, err := in.PVE.Groups(ctx)
	if err != nil {
		return err
	}
	var have []pvecli.Rule
	if !slices.Contains(groups, FirewallGroup) {
		if err := in.run(ctx, "pvesh", "create", "/cluster/firewall/groups", "--group", FirewallGroup, "--comment",
			FirewallComment+": every worker's firewall"); err != nil {
			return err
		}
	} else if have, err = in.PVE.GroupRules(ctx, FirewallGroup); err != nil {
		return err
	}
	if sameRules(have, want) {
		return nil
	}
	path := "/cluster/firewall/groups/" + FirewallGroup
	// Inserted at the top, last first, so they end up in order above the old rules.
	for i := len(want) - 1; i >= 0; i-- {
		if err := in.run(ctx, "pvesh", append([]string{"create", path, "--pos", "0"}, ruleArgs(want[i])...)...); err != nil {
			return err
		}
	}
	// The old rules now follow the new ones; removed from the bottom up, so positions don't shift.
	for pos := len(want) + len(have) - 1; pos >= len(want); pos-- {
		if err := in.run(ctx, "pvesh", "delete", path+"/"+strconv.Itoa(pos)); err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) syncHostRule(ctx context.Context) error {
	node, err := in.nodeName(ctx)
	if err != nil {
		return err
	}
	rules, err := in.PVE.HostRules(ctx, node)
	if err != nil {
		return err
	}
	path := "/nodes/" + node + "/firewall/rules"
	want, found := hostRule(), false
	// From the bottom up, so positions don't shift.
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		if r.Comment != FirewallComment {
			continue
		}
		if sameRule(r, want) && !found {
			found = true
			continue
		}
		if err := in.run(ctx, "pvesh", "delete", path+"/"+strconv.Itoa(r.Pos)); err != nil {
			return err
		}
	}
	if found {
		return nil
	}
	return in.run(ctx, "pvesh", append([]string{"create", path}, ruleArgs(want)...)...)
}

// templateFirewall gives a new runner template its firewall, which every clone copies: on, DHCP allowed, nothing in
// that the worker didn't start, and the worker security group. The template's net0 has firewall=1.
func (in *Installer) templateFirewall(ctx context.Context, vmid string) error {
	node, err := in.nodeName(ctx)
	if err != nil {
		return err
	}
	path := "/nodes/" + node + "/qemu/" + vmid + "/firewall"
	if err := in.run(ctx, "pvesh", "set", path+"/options", "--enable", "1", "--dhcp", "1", "--macfilter", "1",
		"--policy_in", "DROP", "--policy_out", "ACCEPT"); err != nil {
		return err
	}
	return in.run(ctx, "pvesh", "create", path+"/rules", "--type", "group", "--action", FirewallGroup, "--enable", "1")
}

// firewallDrift returns what differs between the worker firewall and what configureFirewall sets up, or nothing.
func (in *Installer) firewallDrift(ctx context.Context, s *settings.Settings) []string {
	var drift []string
	controller := netip.Addr{}
	if vm, ok, err := in.systemVM(ctx, vmtags.Controller); err == nil && ok {
		controller, _ = in.agentIPv4(ctx, vm.VMID)
	}
	if want, err := in.blocked(ctx, s, controller); err != nil {
		drift = append(drift, err.Error())
	} else if have, err := in.PVE.IPSet(ctx, BlockedIPSet); err != nil {
		drift = append(drift, "IP set "+BlockedIPSet+" is missing")
	} else {
		for _, cidr := range want {
			if !slices.Contains(have, cidr) {
				drift = append(drift, "IP set "+BlockedIPSet+" lacks "+cidr)
			}
		}
	}
	dns, err := in.hostDNS(ctx, s)
	if err != nil {
		return append(drift, err.Error())
	}
	if have, err := in.PVE.GroupRules(ctx, FirewallGroup); err != nil {
		drift = append(drift, "security group "+FirewallGroup+" is missing")
	} else if !sameRules(have, workerRules(dns)) {
		drift = append(drift, "security group "+FirewallGroup+" has other rules")
	}
	node, err := in.nodeName(ctx)
	if err != nil {
		return append(drift, err.Error())
	}
	rules, err := in.PVE.HostRules(ctx, node)
	if err != nil || !slices.ContainsFunc(rules, func(r pvecli.Rule) bool {
		return r.Comment == FirewallComment && sameRule(r, hostRule())
	}) {
		drift = append(drift, "the host firewall's DHCP rule for "+VNet+" is missing")
	}
	return drift
}

// removeFirewall removes the host's DHCP rule, the security group, and the IP set. The datacenter firewall stays
// on: it was the user's to turn on.
func (in *Installer) removeFirewall(ctx context.Context) error {
	in.Out.Step("Worker firewall")
	node, err := in.nodeName(ctx)
	if err != nil {
		return err
	}
	if rules, err := in.PVE.HostRules(ctx, node); err == nil {
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].Comment == FirewallComment {
				if err := in.run(ctx, "pvesh", "delete", "/nodes/"+node+"/firewall/rules/"+
					strconv.Itoa(rules[i].Pos)); err != nil {
					return err
				}
			}
		}
	}
	if groups, err := in.PVE.Groups(ctx); err == nil && slices.Contains(groups, FirewallGroup) {
		path := "/cluster/firewall/groups/" + FirewallGroup
		rules, err := in.PVE.GroupRules(ctx, FirewallGroup)
		if err != nil {
			return err
		}
		for i := len(rules) - 1; i >= 0; i-- {
			if err := in.run(ctx, "pvesh", "delete", path+"/"+strconv.Itoa(rules[i].Pos)); err != nil {
				return err
			}
		}
		if err := in.run(ctx, "pvesh", "delete", path); err != nil {
			return err
		}
	}
	if sets, err := in.PVE.IPSets(ctx); err == nil && slices.Contains(sets, BlockedIPSet) {
		if err := in.run(ctx, "pvesh", "delete", "/cluster/firewall/ipset/"+BlockedIPSet, "--force", "1"); err != nil {
			return err
		}
	}
	return nil
}

// errNoClusterFirewall means the datacenter firewall is off, so no worker's firewall would apply.
var errNoClusterFirewall = errors.New("the datacenter firewall is off, so workers would reach the LAN and the " +
	"host: turn it on under Datacenter > Firewall > Options (check first that its rules let you in), then run " +
	"parcon again")
