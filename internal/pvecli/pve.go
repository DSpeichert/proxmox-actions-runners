package pvecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

// PVE reads Proxmox through its command-line tools and runs commands in guests. Changes go through Changer.
type PVE struct {
	Exec Exec
}

func (p PVE) json(ctx context.Context, v any, name string, args ...string) error {
	out, err := p.Exec.Run(ctx, C(name, append(args, "--output-format", "json")...))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("%s %s: decode: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// NodeName returns this node's name.
func (p PVE) NodeName(ctx context.Context) (string, error) {
	var nodes []struct {
		Node string `json:"node"`
	}
	if err := p.json(ctx, &nodes, "pvesh", "get", "/nodes"); err != nil {
		return "", err
	}
	if len(nodes) == 0 || nodes[0].Node == "" {
		return "", errors.New("pvesh get /nodes: no node")
	}
	return nodes[0].Node, nil
}

// VMs returns every QEMU VM and template on node, sorted by VMID.
func (p PVE) VMs(ctx context.Context, node string) ([]proxmox.VM, error) {
	out, err := p.Exec.Run(ctx, C("pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json"))
	if err != nil {
		return nil, err
	}
	return proxmox.ParseResources(out, node)
}

// VMConfig returns a VM's config, each value as a string.
func (p PVE) VMConfig(ctx context.Context, node string, vmid int) (map[string]string, error) {
	var raw map[string]any
	if err := p.json(ctx, &raw, "pvesh", "get", fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)); err != nil {
		return nil, err
	}
	cfg := make(map[string]string, len(raw))
	for k, v := range raw {
		switch v := v.(type) {
		case string:
			cfg[k] = v
		case float64:
			cfg[k] = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			cfg[k] = fmt.Sprint(v)
		}
	}
	return cfg, nil
}

// Pool is a resource pool.
type Pool struct {
	ID      string `json:"poolid"`
	Comment string `json:"comment"`
}

// Pools returns the resource pools.
func (p PVE) Pools(ctx context.Context) ([]Pool, error) {
	var pools []Pool
	err := p.json(ctx, &pools, "pveum", "pool", "list")
	return pools, err
}

// Role is a permission role.
type Role struct {
	ID    string `json:"roleid"`
	Privs string `json:"privs"`
}

// Roles returns the roles.
func (p PVE) Roles(ctx context.Context) ([]Role, error) {
	var roles []Role
	err := p.json(ctx, &roles, "pveum", "role", "list")
	return roles, err
}

// Users returns the user IDs.
func (p PVE) Users(ctx context.Context) ([]string, error) {
	var users []struct {
		ID string `json:"userid"`
	}
	if err := p.json(ctx, &users, "pveum", "user", "list"); err != nil {
		return nil, err
	}
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	return ids, nil
}

// Tokens returns a user's API token IDs, without the user part.
func (p PVE) Tokens(ctx context.Context, user string) ([]string, error) {
	var tokens []struct {
		ID string `json:"tokenid"`
	}
	if err := p.json(ctx, &tokens, "pveum", "user", "token", "list", user); err != nil {
		return nil, err
	}
	ids := make([]string, len(tokens))
	for i, t := range tokens {
		ids[i] = t.ID
	}
	return ids, nil
}

// ACL is one access control entry. Type is "user", "group", or "token".
type ACL struct {
	Path string `json:"path"`
	Type string `json:"type"`
	UGID string `json:"ugid"`
	Role string `json:"roleid"`
}

// ACLs returns every access control entry.
func (p PVE) ACLs(ctx context.Context) ([]ACL, error) {
	var acls []ACL
	err := p.json(ctx, &acls, "pveum", "acl", "list")
	return acls, err
}

// SDNExists reports whether an SDN object, such as "zones/parzone" or "vnets/parnet", exists.
func (p PVE) SDNExists(ctx context.Context, path string) bool {
	_, err := p.Exec.Run(ctx, C("pvesh", "get", "/cluster/sdn/"+path, "--output-format", "json"))
	return err == nil
}

// PendingSDN returns the SDN zones and VNets with changes that aren't applied yet, other than ours. Applying the SDN
// config applies all of them.
func (p PVE) PendingSDN(ctx context.Context, ours ...string) ([]string, error) {
	var pending []string
	for _, kind := range []string{"zone", "vnet"} {
		var objs []map[string]any
		if err := p.json(ctx, &objs, "pvesh", "get", "/cluster/sdn/"+kind+"s", "--pending", "1"); err != nil {
			return nil, err
		}
		for _, o := range objs {
			id, _ := o[kind].(string)
			if state, _ := o["state"].(string); state != "" && id != "" && !slices.Contains(ours, id) {
				pending = append(pending, id)
			}
		}
	}
	return pending, nil
}

// SDNObject returns an SDN object's config, such as "zones/parzone".
func (p PVE) SDNObject(ctx context.Context, path string) (map[string]any, error) {
	var obj map[string]any
	err := p.json(ctx, &obj, "pvesh", "get", "/cluster/sdn/"+path)
	return obj, err
}

// DHCPRange is one of a subnet's DHCP ranges.
type DHCPRange struct {
	Start string `json:"start-address"`
	End   string `json:"end-address"`
}

// Subnet is a VNet's subnet.
type Subnet struct {
	// ID is Proxmox's name for it, such as parzone-10.251.0.0-22.
	ID        string      `json:"subnet"`
	CIDR      string      `json:"cidr"`
	Gateway   string      `json:"gateway"`
	SNAT      int         `json:"snat"`
	DHCPRange []DHCPRange `json:"dhcp-range"`
	DNSServer string      `json:"dhcp-dns-server"`
}

// Subnets returns a VNet's subnets.
func (p PVE) Subnets(ctx context.Context, vnet string) ([]Subnet, error) {
	var subnets []Subnet
	err := p.json(ctx, &subnets, "pvesh", "get", "/cluster/sdn/vnets/"+vnet+"/subnets")
	return subnets, err
}

// ClusterFirewall reports whether the datacenter firewall is on. Without it, no guest's firewall rules apply.
func (p PVE) ClusterFirewall(ctx context.Context) (bool, error) {
	var opts struct {
		Enable int `json:"enable"`
	}
	err := p.json(ctx, &opts, "pvesh", "get", "/cluster/firewall/options")
	return opts.Enable == 1, err
}

// DNS returns the node's DNS servers, in order.
func (p PVE) DNS(ctx context.Context, node string) ([]string, error) {
	var dns map[string]any
	if err := p.json(ctx, &dns, "pvesh", "get", "/nodes/"+node+"/dns"); err != nil {
		return nil, err
	}
	var servers []string
	for _, k := range []string{"dns1", "dns2", "dns3"} {
		if s, _ := dns[k].(string); s != "" {
			servers = append(servers, s)
		}
	}
	return servers, nil
}

// Rule is a firewall rule, in a security group or on the host.
type Rule struct {
	Pos     int    `json:"pos"`
	Type    string `json:"type"`
	Action  string `json:"action"`
	Proto   string `json:"proto,omitempty"`
	Dport   string `json:"dport,omitempty"`
	Dest    string `json:"dest,omitempty"`
	Iface   string `json:"iface,omitempty"`
	Comment string `json:"comment,omitempty"`
	Enable  int    `json:"enable"`
}

// Groups returns the datacenter firewall's security groups.
func (p PVE) Groups(ctx context.Context) ([]string, error) {
	var groups []struct {
		Group string `json:"group"`
	}
	if err := p.json(ctx, &groups, "pvesh", "get", "/cluster/firewall/groups"); err != nil {
		return nil, err
	}
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = g.Group
	}
	return names, nil
}

// GroupRules returns a security group's rules, in order.
func (p PVE) GroupRules(ctx context.Context, group string) ([]Rule, error) {
	return p.rules(ctx, "/cluster/firewall/groups/"+group)
}

// HostRules returns the node's host firewall rules, in order.
func (p PVE) HostRules(ctx context.Context, node string) ([]Rule, error) {
	return p.rules(ctx, "/nodes/"+node+"/firewall/rules")
}

func (p PVE) rules(ctx context.Context, path string) ([]Rule, error) {
	var rules []Rule
	if err := p.json(ctx, &rules, "pvesh", "get", path); err != nil {
		return nil, err
	}
	slices.SortFunc(rules, func(a, b Rule) int { return a.Pos - b.Pos })
	return rules, nil
}

// IPSets returns the datacenter firewall's IP sets.
func (p PVE) IPSets(ctx context.Context) ([]string, error) {
	var sets []struct {
		Name string `json:"name"`
	}
	if err := p.json(ctx, &sets, "pvesh", "get", "/cluster/firewall/ipset"); err != nil {
		return nil, err
	}
	names := make([]string, len(sets))
	for i, s := range sets {
		names[i] = s.Name
	}
	return names, nil
}

// IPSet returns the CIDRs in a datacenter IP set.
func (p PVE) IPSet(ctx context.Context, name string) ([]string, error) {
	var entries []struct {
		CIDR string `json:"cidr"`
	}
	if err := p.json(ctx, &entries, "pvesh", "get", "/cluster/firewall/ipset/"+name); err != nil {
		return nil, err
	}
	cidrs := make([]string, len(entries))
	for i, e := range entries {
		cidrs[i] = e.CIDR
	}
	return cidrs, nil
}

// StorageStatus is a storage's status on the node.
type StorageStatus struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Active  int    `json:"active"`
	Enabled int    `json:"enabled"`
	Avail   int64  `json:"avail"`
}

// Storage returns a storage's status on node.
func (p PVE) Storage(ctx context.Context, node, storage string) (StorageStatus, error) {
	var st StorageStatus
	err := p.json(ctx, &st, "pvesh", "get", fmt.Sprintf("/nodes/%s/storage/%s/status", node, storage))
	return st, err
}

// NextID returns the next free VMID that Proxmox suggests.
func (p PVE) NextID(ctx context.Context) (int, error) {
	out, err := p.Exec.Run(ctx, C("pvesh", "get", "/cluster/nextid"))
	if err != nil {
		return 0, err
	}
	id, err := strconv.Atoi(strings.Trim(strings.TrimSpace(string(out)), `"`))
	if err != nil {
		return 0, fmt.Errorf("pvesh get /cluster/nextid: %q is not a VMID", out)
	}
	return id, nil
}

// AgentPing reports whether a VM's guest agent answers.
func (p PVE) AgentPing(ctx context.Context, vmid int) error {
	_, err := p.Exec.Run(ctx, C("qm", "guest", "cmd", strconv.Itoa(vmid), "ping"))
	return err
}

// ErrExecTimeout is a guest command that didn't finish in time.
var ErrExecTimeout = errors.New("didn't finish in time")

// GuestExec runs argv in a VM through the guest agent and waits up to timeout for it to finish. stdin, which may
// carry secrets, goes to the command and nowhere else. A non-zero exit status is not an error: check the result.
func (p PVE) GuestExec(ctx context.Context, vmid int, timeout time.Duration, argv []string, stdin []byte) (
	proxmox.ExecResult, error,
) {
	if len(stdin) > proxmox.MaxAgentStdinBytes {
		return proxmox.ExecResult{}, fmt.Errorf("guest exec %s in VM %d: stdin is %d bytes, more than %d", argv[0],
			vmid, len(stdin), proxmox.MaxAgentStdinBytes)
	}
	args := []string{"guest", "exec", strconv.Itoa(vmid), "--timeout", strconv.Itoa(int(timeout.Seconds()))}
	if stdin != nil {
		args = append(args, "--pass-stdin", "1")
	}
	cmd := C("qm", append(append(args, "--"), argv...)...)
	cmd.Stdin = stdin
	out, err := p.Exec.Run(ctx, cmd)
	if err != nil {
		// qm may exit non-zero with the command, and still print its status; a status is the answer.
		if res, exited, perr := proxmox.ParseAgentExecStatus(out); perr == nil && exited {
			return res, nil
		}
		return proxmox.ExecResult{}, fmt.Errorf("guest exec in VM %d: %w", vmid, err)
	}
	res, exited, err := proxmox.ParseAgentExecStatus(out)
	if err != nil {
		return proxmox.ExecResult{}, fmt.Errorf("guest exec %s in VM %d: %w", argv[0], vmid, err)
	}
	if !exited {
		return res, fmt.Errorf("guest exec %s in VM %d: %w (%s)", argv[0], vmid, ErrExecTimeout, timeout)
	}
	return res, nil
}
