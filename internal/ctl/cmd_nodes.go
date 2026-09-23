package ctl

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/platteration/ewastesavior/internal/proto"
)

// nodeStatus summarizes a node's state in one word.
func nodeStatus(n proto.NodeView) string {
	switch {
	case n.Liveness != proto.NodeOnline:
		return "offline"
	case !n.Approved:
		return "pending"
	case n.Quarantine != "":
		return "quarantined"
	case n.Drain:
		return "draining"
	case n.Status.State != "":
		return string(n.Status.State)
	}
	return "online"
}

func rolesString(roles []proto.Role) string {
	s := make([]string, len(roles))
	for i, r := range roles {
		s[i] = string(r)
	}
	return strings.Join(s, ",")
}

func cmdNodes(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("nodes", "[--all]", "List online and pending nodes (--all: offline ones too).")
	all := f.Bool("all", false, "include offline nodes")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return a.fail(err)
	}
	shown := make([]proto.NodeView, 0, len(nodes))
	hidden := 0
	for _, n := range nodes {
		if *all || n.Liveness == proto.NodeOnline {
			shown = append(shown, n)
		} else {
			hidden++
		}
	}
	sort.Slice(shown, func(i, j int) bool {
		if shown[i].Name != shown[j].Name {
			return shown[i].Name < shown[j].Name
		}
		return shown[i].ID < shown[j].ID
	})
	if a.g.json {
		return a.printJSON(shown)
	}
	now := a.now()
	t := newTable(a.stdout, "NAME", "ID", "CODE", "STATUS", "ROLES", "CPU", "CORES", "MEM", "TEMP", "BATT", "TASKS", "DISPLAY", "ADDR")
	for _, n := range shown {
		m := n.Status.Metrics
		status := nodeStatus(n)
		if status == "offline" {
			status += " " + fmtAge(n.LastSeen, now)
		}
		cpu, temp, batt := "", "", ""
		if n.Liveness == proto.NodeOnline {
			cpu = fmt.Sprintf("%.0f%%", m.CPUPercent)
			if m.CPUTempC > 0 {
				temp = fmt.Sprintf("%.0fC", m.CPUTempC)
			}
			if m.BatteryPercent >= 0 && n.Inventory.HasBattery {
				batt = fmt.Sprintf("%d%%", m.BatteryPercent)
				if m.OnBattery {
					batt += "*"
				}
			}
		}
		t.row(n.Name, n.ID, n.ShortCode, status, rolesString(n.Roles), cpu,
			fmtCores(n.Allocated.Cores)+"/"+fmtCores(n.Status.Total.Cores),
			fmt.Sprintf("%d/%d", n.Allocated.MemMB, n.Status.Total.MemMB),
			temp, batt, strconv.Itoa(len(n.RunningTasks)), displaySummary(n), n.Addr)
	}
	t.flush()
	if hidden > 0 {
		fmt.Fprintf(a.stderr, "%d offline node(s) hidden; --all shows them.\n", hidden)
	}
	return 0
}

func displaySummary(n proto.NodeView) string {
	if !proto.HasRole(n.Roles, proto.RoleDisplay) {
		return ""
	}
	if n.WallID != "" {
		return "wall:" + n.WallID
	}
	if n.Display != nil {
		return n.Display.Mode
	}
	if n.Status.Display.Mode != "" {
		return n.Status.Display.Mode
	}
	return "status"
}

func cmdNode(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("node", "<ref>", "Show everything the hive knows about a node (ref = node ID or name).")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	n, err := c.Node(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(n)
	}
	a.printNode(n)
	return 0
}

func (a *app) printNode(n proto.NodeView) {
	now := a.now()
	inv, st, m := n.Inventory, n.Status, n.Status.Metrics
	k := newKV(a.stdout)
	k.add("Name", fmt.Sprintf("%s (id %s, code %s)", n.Name, n.ID, n.ShortCode))
	status := nodeStatus(n)
	if st.Reason != "" {
		status += ": " + st.Reason
	}
	if n.Quarantine != "" {
		status += " (quarantine: " + n.Quarantine + ")"
	}
	k.add("Status", status)
	k.add("Seen", fmt.Sprintf("first %s, last %s", fmtTime(n.FirstSeen), fmtAge(n.LastSeen, now)))
	k.add("Address", strings.TrimSpace(n.Addr+" "+strings.Join(st.Addrs, " ")))
	k.add("Version", n.Version)
	k.add("Roles", rolesString(n.Roles))
	if len(n.Labels) > 0 {
		k.add("Labels", sortedKV(n.Labels))
	}
	if len(n.AdminLabels) > 0 {
		k.add("Admin labels", sortedKV(n.AdminLabels))
	}
	hw := strings.TrimSpace(inv.Vendor + " " + inv.Product)
	cpu := inv.CPUModel
	if inv.CPUMHz > 0 {
		cpu += fmt.Sprintf(" @ %d MHz", inv.CPUMHz)
	}
	k.add("Machine", hw)
	k.add("CPU", fmt.Sprintf("%s, %d threads, %s (%s)", cpu, inv.Cores, inv.Arch, inv.MachineArch))
	k.add("Memory", fmt.Sprintf("%s RAM, %s swap", fmtMB(inv.MemTotalMB), fmtMB(inv.SwapTotalMB)))
	k.add("Kernel", strings.TrimSpace(inv.Kernel+" "+inv.OSVersion))
	iso := "no full isolation"
	if n.FullIsolation {
		iso = "full isolation"
	}
	k.add("Sandbox", fmt.Sprintf("%s (%s) %s", n.Sandbox, iso, strings.Join(n.SandboxCaps, " ")))
	scratch := "disk"
	if n.ScratchInRAM {
		scratch = "RAM"
	}
	k.add("Resources", fmt.Sprintf("total %s cores / %s / disk %s (scratch in %s); allocated %s cores / %s",
		fmtCores(st.Total.Cores), fmtMB(st.Total.MemMB), fmtMB(st.Total.DiskMB), scratch,
		fmtCores(n.Allocated.Cores), fmtMB(n.Allocated.MemMB)))
	if n.Liveness == proto.NodeOnline {
		met := fmt.Sprintf("cpu %.0f%%, load %.2f, %s available", m.CPUPercent, m.Load1, fmtMB(m.MemAvailableMB))
		if m.CPUTempC > 0 {
			met += fmt.Sprintf(", %.0f C (limit %.0f)", m.CPUTempC, m.CPUTempLimitC)
		}
		if m.BatteryPercent >= 0 && inv.HasBattery {
			met += fmt.Sprintf(", battery %d%% %s", m.BatteryPercent, m.BatteryStatus)
		}
		if m.LidClosed {
			met += ", lid closed"
		}
		k.add("Metrics", met)
	}
	if len(n.RunningTasks) > 0 {
		k.add("Tasks", strings.Join(n.RunningTasks, " "))
	}
	if n.ReservedFor != "" {
		k.add("Reserved for", n.ReservedFor)
	}
	if proto.HasRole(n.Roles, proto.RoleDisplay) || st.Display.Active {
		d := st.Display
		disp := displaySummary(n)
		if d.Active {
			disp += fmt.Sprintf(", %dx%d %s via %s, rotate %d", d.FBWidth, d.FBHeight, d.Format, d.Driver, n.DisplayRotate)
		}
		if d.Blanked {
			disp += ", blanked (" + d.BlankReason + ")"
		}
		if d.Error != "" {
			disp += ", error: " + d.Error
		}
		k.add("Display", disp)
		if len(d.MediaErrors) > 0 {
			k.add("Media errors", strings.Join(d.MediaErrors, "; "))
		}
	}
	k.flush()
	for _, w := range n.Warnings {
		fmt.Fprintf(a.stdout, "Warning: %s\n", sanitizeCell(w))
	}
}

func cmdRename(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("rename", "<ref> <name>", "Rename a node. Names use lowercase letters, digits and dashes (1-32 characters).")
	pos, code, ok := a.parse(f, args, 2, 2)
	if !ok {
		return code
	}
	name := strings.ToLower(pos[1])
	if !proto.ValidNodeName(name) {
		return a.usageError("invalid name %q: use 1-32 lowercase letters, digits and inner dashes, not shaped like a node ID", sanitizeCell(pos[1]))
	}
	return a.patchNode(ctx, pos[0], proto.NodePatch{Name: &name}, func(n proto.NodeView) string {
		return fmt.Sprintf("node %s is now named %s", sanitizeCell(n.ID), sanitizeCell(n.Name))
	})
}

// patchNode applies p and prints msg(result) (or the node as JSON).
func (a *app) patchNode(ctx context.Context, ref string, p proto.NodePatch, msg func(proto.NodeView) string) int {
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	n, err := c.PatchNode(ctx, ref, p)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(n)
	}
	fmt.Fprintln(a.stdout, msg(n))
	return 0
}

func cmdLabel(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("label", "<ref> [k=v | k-]... [--clear]",
		"Set (k=v) or remove (k-) admin labels. Admin labels override the node's own labels key by key.")
	clearAll := f.Bool("clear", false, "remove all admin labels first")
	pos, code, ok := a.parse(f, args, 1, -1)
	if !ok {
		return code
	}
	if len(pos) == 1 && !*clearAll {
		return a.usageError("give labels to set (k=v) or remove (k-), or --clear")
	}
	type change struct {
		key, value string
		remove     bool
	}
	var changes []change
	for _, s := range pos[1:] {
		if k, v, ok := strings.Cut(s, "="); ok {
			if err := checkLabel(k, v); err != nil {
				return a.usageError("%v", err)
			}
			changes = append(changes, change{key: k, value: v})
		} else if k, ok := strings.CutSuffix(s, "-"); ok && proto.ValidLabelKey(k) {
			changes = append(changes, change{key: k, remove: true})
		} else {
			return a.usageError("%q is neither k=v nor k-", sanitizeCell(s))
		}
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	labels := map[string]string{}
	if !*clearAll {
		n, err := c.Node(ctx, pos[0])
		if err != nil {
			return a.fail(err)
		}
		for k, v := range n.AdminLabels {
			labels[k] = v
		}
	}
	for _, ch := range changes {
		if ch.remove {
			delete(labels, ch.key)
		} else {
			labels[ch.key] = ch.value
		}
	}
	if len(labels) > proto.MaxLabels {
		return a.fail(fmt.Errorf("a node can have at most %d labels", proto.MaxLabels))
	}
	return a.patchNode(ctx, pos[0], proto.NodePatch{Labels: &labels}, func(n proto.NodeView) string {
		return fmt.Sprintf("node %s labels: %s", describeNode(n), sanitizeCell(sortedKV(n.Labels)))
	})
}

// checkLabel validates a label key and value.
func checkLabel(k, v string) error {
	if !proto.ValidLabelKey(k) {
		return fmt.Errorf("invalid label key %q (lowercase letters, digits, _ . - /; up to 63 characters)", sanitizeCell(k))
	}
	if len(v) > 128 || !utf8.ValidString(v) || sanitizeCell(v) != v {
		return fmt.Errorf("invalid value for label %q (at most 128 printable characters)", k)
	}
	return nil
}

// nodePatchCmd returns the drain, undrain, approve and unquarantine commands.
func nodePatchCmd(kind string) func(a *app, ctx context.Context, args []string) int {
	return func(a *app, ctx context.Context, args []string) int {
		var p proto.NodePatch
		var help, done string
		yes, no := true, false
		switch kind {
		case "drain":
			p.Drain, help, done = &yes, "Stop giving the node new tasks; running tasks finish.", "draining"
		case "undrain":
			p.Drain, help, done = &no, "Let a drained node take tasks again.", "taking tasks again"
		case "approve":
			p.Approved, help, done = &yes, "Approve a pending node so it can join the swarm.", "approved"
		case "unquarantine":
			p.ClearQuarantine, help, done = true, "Clear a node's quarantine so it gets tasks again.", "no longer quarantined"
		}
		f := a.flagSet(kind, "<ref>", help)
		pos, code, ok := a.parse(f, args, 1, 1)
		if !ok {
			return code
		}
		return a.patchNode(ctx, pos[0], p, func(n proto.NodeView) string {
			return fmt.Sprintf("node %s: %s", describeNode(n), done)
		})
	}
}

func cmdRotate(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("rotate", "<ref> <0|90|180|270>", "Rotate a node's screen clockwise (how the monitor is mounted).")
	pos, code, ok := a.parse(f, args, 2, 2)
	if !ok {
		return code
	}
	deg, err := strconv.Atoi(pos[1])
	if err != nil || (deg != 0 && deg != 90 && deg != 180 && deg != 270) {
		return a.usageError("rotation must be 0, 90, 180 or 270")
	}
	return a.patchNode(ctx, pos[0], proto.NodePatch{DisplayRotate: &deg}, func(n proto.NodeView) string {
		return fmt.Sprintf("node %s: screen rotated %d degrees", describeNode(n), n.DisplayRotate)
	})
}

func cmdIdentify(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("identify", "[<ref>...|--all] [--seconds N]", "Flash the screen, blink the keyboard lights and beep on nodes, so you can find them.")
	all := f.Bool("all", false, "identify every online node")
	seconds := f.Int("seconds", 30, "how long (1-600)")
	pos, code, ok := a.parse(f, args, 0, -1)
	if !ok {
		return code
	}
	if len(pos) == 0 && !*all {
		return a.usageError("name the nodes to identify, or pass --all")
	}
	if len(pos) > 0 && *all {
		return a.usageError("give node refs or --all, not both")
	}
	if *seconds < 1 || *seconds > 600 {
		return a.usageError("--seconds must be 1..600")
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if err := c.Identify(ctx, proto.IdentifyRequest{Nodes: pos, Seconds: *seconds}); err != nil {
		return a.fail(err)
	}
	if !a.g.json {
		who := "all online nodes"
		if len(pos) > 0 {
			who = sanitizeCell(strings.Join(pos, ", "))
		}
		fmt.Fprintf(a.stdout, "Identifying %s for %d s.\n", who, *seconds)
	}
	return 0
}

// nodeActionCmd returns the reboot and poweroff commands.
func nodeActionCmd(action string) func(a *app, ctx context.Context, args []string) int {
	return func(a *app, ctx context.Context, args []string) int {
		f := a.flagSet(action, "<ref>", "Ask a node to "+action+". The node acknowledges first, so it happens only once.")
		pos, code, ok := a.parse(f, args, 1, 1)
		if !ok {
			return code
		}
		c, err := a.connect(ctx)
		if err != nil {
			return a.fail(err)
		}
		if err := c.Action(ctx, pos[0], proto.NodeAction{Action: action}); err != nil {
			return a.fail(err)
		}
		if !a.g.json {
			fmt.Fprintf(a.stdout, "Sent %s to %s; it happens at the node's next heartbeat.\n", action, sanitizeCell(pos[0]))
		}
		return 0
	}
}

func cmdForget(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("forget", "<ref>", "Remove an offline node from the hive. If it comes back it joins as a new node.")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if err := c.DeleteNode(ctx, pos[0]); err != nil {
		return a.fail(err)
	}
	if !a.g.json {
		fmt.Fprintf(a.stdout, "Forgot node %s.\n", sanitizeCell(pos[0]))
	}
	return 0
}
