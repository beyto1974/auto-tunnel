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

	active, listening, degraded, broken := m.snap.Counts()
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
		return dimStyle.Render(fmt.Sprintf("  no tunnels match %q", m.filter))
	}

	const (
		stateW = 12
		nameW  = 22
		localW = 12
		remW   = 22
		connW  = 7
		byteW  = 10
	)
	imageW := m.width - (stateW + nameW + localW + remW + connW + 2*byteW + 8)
	imageW = clamp(imageW, 10, 34)

	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf(
		"%-*s %-*s %-*s %-*s %-*s %*s %*s %*s",
		stateW, "STATE", nameW, "CONTAINER", imageW, "IMAGE",
		localW, "LOCAL", remW, "REMOTE", connW, "CONNS", byteW, "IN", byteW, "OUT")))
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
		line := fmt.Sprintf(
			"%-*s %-*s %-*s %-*s %-*s %*d %*s %*s",
			stateW, t.State, nameW, truncate(t.Name, nameW), imageW, truncate(t.Image, imageW),
			localW, local, remW, truncate(t.RemoteTarget, remW),
			connW, t.ActiveConns, byteW, humanBytes(t.BytesIn), byteW, humanBytes(t.BytesOut))

		switch {
		case i == m.cursor:
			b.WriteString(selectedStyle.Render(line))
		default:
			// Colour only the state column so the row stays readable.
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
	if row, ok := m.selected(); ok && row.LastError != "" {
		b.WriteString(dimStyle.Render("  " + row.Name + ": " + truncate(row.LastError, max(20, m.width-6))))
		b.WriteString("\n")
	}
	return b.String()
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
	help := dimStyle.Render("↑/↓ select · p pause · r rescan · s sort (" + m.sort.String() + ") · / filter · l log · q quit")
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
