package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
	headerStyle   = lipgloss.NewStyle().Bold(true).Underline(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	bannerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("197"))

	stateStyles = map[state.TunnelState]lipgloss.Style{
		state.TunnelActive:      lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		state.TunnelListening:   lipgloss.NewStyle().Foreground(lipgloss.Color("39")),
		state.TunnelDegraded:    lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		state.TunnelPaused:      lipgloss.NewStyle().Faint(true),
		state.TunnelError:       lipgloss.NewStyle().Foreground(lipgloss.Color("197")),
		state.TunnelUnsupported: lipgloss.NewStyle().Faint(true),
		state.TunnelOffered:     lipgloss.NewStyle().Faint(true),
	}

	sshStateStyles = map[sshconn.State]lipgloss.Style{
		sshconn.StateConnected:    lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		sshconn.StateConnecting:   lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		sshconn.StateReconnecting: lipgloss.NewStyle().Foreground(lipgloss.Color("197")),
		sshconn.StateClosed:       lipgloss.NewStyle().Faint(true),
	}
)

// View renders the dashboard.
func (m Model) View() string {
	var b strings.Builder

	b.WriteString(m.header())
	b.WriteString("\n")
	if banner := m.banner(); banner != "" {
		b.WriteString(banner + "\n")
	}
	b.WriteString("\n")
	b.WriteString(m.table())
	b.WriteString("\n")
	if m.showLog {
		b.WriteString(m.logPane())
		b.WriteString("\n")
	}
	b.WriteString(m.footer())
	return b.String()
}

func (m Model) header() string {
	ssh := m.snap.SSH
	sshText := string(ssh.State)
	if ssh.State == sshconn.StateConnected && ssh.RTT > 0 {
		sshText += fmt.Sprintf(" %s", roundRTT(ssh.RTT))
	}
	if ssh.State == sshconn.StateReconnecting && ssh.Attempts > 0 {
		sshText += fmt.Sprintf(" (attempt %d)", ssh.Attempts)
	}
	style, ok := sshStateStyles[ssh.State]
	if !ok {
		style = dimStyle
	}

	active, listening, degraded, broken, offered := m.snap.Counts()
	parts := []string{
		titleStyle.Render("auto-tunnel"),
		m.snap.Target,
		"ssh " + style.Render(sshText),
		"up " + shortDuration(time.Since(m.snap.Started)),
		"next scan " + nextScanText(m.snap.NextScan),
		fmt.Sprintf("%d container(s)", m.snap.Containers),
		fmt.Sprintf("%d tunnel(s): %d active, %d idle, %d degraded, %d broken",
			len(m.snap.Tunnels), active, listening, degraded, broken),
	}
	if offered > 0 {
		parts = append(parts, fmt.Sprintf("%d available", offered))
	}
	if source := sourceFilters[m.source]; source != "" {
		parts = append(parts, "showing "+source)
	}
	if m.onlyOn {
		parts = append(parts, "forwarded only")
	}
	return strings.Join(parts, dimStyle.Render(" · "))
}

func (m Model) banner() string {
	switch {
	case m.snap.DiscoveryError != "":
		return bannerStyle.Render("docker discovery failing: " + m.snap.DiscoveryError)
	case m.snap.SSH.State != sshconn.StateConnected && m.snap.SSH.LastError != "":
		return bannerStyle.Render("ssh: " + m.snap.SSH.LastError)
	}
	return ""
}

// columns are laid out to a fixed width, with the image column absorbing
// whatever space is left over.
func (m Model) table() string {
	if len(m.snap.Tunnels) == 0 {
		return dimStyle.Render("  no forwardable ports discovered yet")
	}
	if len(m.rows) == 0 {
		return dimStyle.Render(fmt.Sprintf("  no tunnels match %q", m.describeFilter()))
	}

	const (
		stateW = 12
		srcW   = 6
		nameW  = 22
		localW = 12
		// Wide enough for https://127.0.0.1:15432, the longest URL this can
		// produce, so a web row never has its address cut short.
		remW  = 24
		connW = 7
		byteW = 10
	)
	// DETAIL holds whatever describes the row beside its name: the image for a
	// container, the bound address for a host socket, the spec for a declared
	// forward.
	detailW := m.width - (stateW + srcW + nameW + localW + remW + connW + 2*byteW + 9)
	detailW = clamp(detailW, 10, 34)

	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf(
		"%-*s %-*s %-*s %-*s %-*s %-*s %*s %*s %*s",
		stateW, "STATE", srcW, "SRC", nameW, "NAME", detailW, "DETAIL",
		localW, "LOCAL", remW, "REMOTE / URL", connW, "CONNS", byteW, "IN", byteW, "OUT")))
	b.WriteString("\n")

	visible := m.visibleRowCount()
	start := 0
	if m.cursor >= visible {
		start = m.cursor - visible + 1
	}
	end := min(len(m.rows), start+visible)

	for i := start; i < end; i++ {
		t := m.rows[i]
		local := "-"
		if t.LocalPort != 0 {
			local = strconv.Itoa(t.LocalPort)
		}
		// An empty source is a docker row: discovery leaves the field at its
		// zero value there, and a blank column would read as a bug.
		source := t.Source
		if source == "" {
			source = "docker"
		}
		// A web service shows the address to open rather than the one it points
		// at: the far side is in the log and on the selected-row line, while the
		// URL is the thing the user is here to click.
		remote := t.RemoteTarget
		if t.URL != "" {
			remote = t.URL
		}
		// The URL is printed as plain text rather than wrapped in an OSC 8
		// hyperlink: terminals that honour that escape underline it, and a
		// dotted rule through the table costs more than it buys. Terminals that
		// detect bare URLs still make it clickable, and "o" opens it anywhere.
		remoteCell := fmt.Sprintf("%-*s", remW, truncate(remote, remW))
		line := fmt.Sprintf(
			"%-*s %-*s %-*s %-*s %-*s %s %*d %*s %*s",
			stateW, t.State, srcW, truncate(source, srcW),
			nameW, truncate(t.Name, nameW), detailW, truncate(t.Image, detailW),
			localW, local, remoteCell,
			connW, t.ActiveConns, byteW, humanBytes(t.BytesIn), byteW, humanBytes(t.BytesOut))

		switch {
		case i == m.cursor:
			b.WriteString(selectedStyle.Render(line))
		default:
			// Colour only the state column so the row stays readable. The link
			// lives further along the line, so this slice never splits it.
			style, ok := stateStyles[t.State]
			if !ok {
				style = dimStyle
			}
			b.WriteString(style.Render(line[:stateW]) + line[stateW:])
		}
		b.WriteString("\n")
	}

	if end < len(m.rows) || start > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  showing %d-%d of %d\n", start+1, end, len(m.rows))))
	}
	if row, ok := m.selected(); ok {
		if row.LastError != "" {
			b.WriteString(dimStyle.Render("  " + row.Name + ": " + truncate(row.LastError, max(20, m.width-6))))
			b.WriteString("\n")
		}
		if row.URL != "" {
			// The far side is only shown here, now that the column holds the URL.
			b.WriteString(dimStyle.Render("  open: " + row.URL + "  (o)   via " + row.RemoteTarget))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// onOff renders a toggle for the footer.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// describeFilter names whatever is narrowing the table, so an empty result can
// explain itself whether the cause was the text filter, the source key, or both.
func (m Model) describeFilter() string {
	parts := []string{}
	if m.onlyOn {
		parts = append(parts, "forwarded")
	}
	if source := sourceFilters[m.source]; source != "" {
		parts = append(parts, source)
	}
	if m.filter != "" {
		parts = append(parts, m.filter)
	}
	return strings.Join(parts, " ")
}

func (m Model) logPane() string {
	lines := m.logs.Tail(m.logHeight())
	if len(lines) == 0 {
		return dimStyle.Render("  (no log output yet)")
	}
	var b strings.Builder
	b.WriteString(headerStyle.Render("LOG"))
	b.WriteString("\n")
	for _, line := range lines {
		b.WriteString(dimStyle.Render("  " + truncate(line, max(20, m.width-2))))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) footer() string {
	if m.editing {
		return titleStyle.Render("filter: ") + m.filter + dimStyle.Render("   (enter to apply, esc to clear)")
	}
	help := dimStyle.Render("↑/↓ select · enter on/off · o open · f forwarded (" + onOff(m.onlyOn) + ") · a all shown · t source (" +
		sourceFilterName(sourceFilters[m.source]) + ") · p pause · r rescan · s sort (" +
		m.sort.String() + ") · / filter · l log · q quit")
	if m.filter != "" {
		help = dimStyle.Render("filter "+strconv.Quote(m.filter)+" · ") + help
	}
	if m.status != "" {
		help += dimStyle.Render("   " + m.status)
	}
	return help
}

// visibleRowCount is how many table rows fit, after the header, footer, banner,
// and optional log pane have taken their share.
func (m Model) visibleRowCount() int {
	chrome := 6 // title, blank, column header, footer, and slack
	if m.banner() != "" {
		chrome++
	}
	if m.showLog {
		chrome += m.logHeight() + 1
	}
	return max(1, m.height-chrome)
}

func (m Model) logHeight() int {
	return clamp(m.height/4, 3, 12)
}

func nextScanText(next time.Time) string {
	if next.IsZero() {
		return "-"
	}
	d := time.Until(next)
	if d <= 0 {
		return "now"
	}
	return shortDuration(d)
}

func roundRTT(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(100 * time.Microsecond)
	}
	return d.Round(time.Millisecond)
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTP"[exp])
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return s[:width-1] + "…"
}

func clamp(v, lo, hi int) int {
	return max(lo, min(hi, v))
}
