// Package launchd installs and manages the lopend LaunchAgent on macOS. It is
// used for from-source installs; a Homebrew install manages the daemon via
// `brew services` instead.
package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const Label = "com.jaxonwang.lopend"

// plistPath returns ~/Library/LaunchAgents/<label>.plist.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// Install writes the LaunchAgent plist pointing at the given daemon binary and
// config, then (re)loads it. logPath receives daemon stdout/stderr.
func Install(daemonBin, configPath, logPath string) error {
	pl, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pl), 0o755); err != nil {
		return err
	}
	args := []string{daemonBin}
	if configPath != "" {
		args = append(args, "-config", configPath)
	}
	body := plist(args, logPath)
	if err := os.WriteFile(pl, []byte(body), 0o644); err != nil {
		return err
	}

	// Reload: bootout (ignore error if not loaded) then bootstrap.
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", uid+"/"+Label).Run()
	if err := exec.Command("launchctl", "bootstrap", uid, pl).Run(); err != nil {
		// Older macOS: fall back to load -w.
		if lerr := exec.Command("launchctl", "load", "-w", pl).Run(); lerr != nil {
			return fmt.Errorf("launchctl bootstrap failed: %w", err)
		}
	}
	return nil
}

// Uninstall stops and removes the LaunchAgent.
func Uninstall() error {
	pl, err := plistPath()
	if err != nil {
		return err
	}
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", uid+"/"+Label).Run()
	_ = exec.Command("launchctl", "unload", pl).Run()
	return os.Remove(pl)
}

func plist(args []string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + xmlEsc(Label) + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range args {
		b.WriteString("    <string>" + xmlEsc(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	if logPath != "" {
		b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEsc(logPath) + "</string>\n")
		b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEsc(logPath) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
