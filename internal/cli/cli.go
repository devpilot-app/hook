// Package cli 实现 devpilot-hook 的所有子命令。
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
)

// Version 由构建时注入（goreleaser -ldflags）。
var Version = "dev"

// Run 是命令入口，返回进程退出码。
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "hook":
		return runHook(args[1:], stdin, stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "install":
		return runInstall(args[1:], stdout, stderr)
	case "uninstall":
		return runUninstall(args[1:], stdout, stderr)
	case "pair":
		return runPair(args[1:], stdout, stderr)
	case "unpair":
		return runUnpair(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "service":
		return runService(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, Version)
		return 0
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `devpilot-hook - local hook daemon for DevPilot

usage:
  devpilot-hook install    [--agent claude,codex,qwen,gemini,grok,cursor,copilot,opencode] [--approval-mode permission-request|pre-tool-use] [--dir PATH]
  devpilot-hook uninstall  [--agent ...] [--purge] [--dir PATH]
  devpilot-hook pair       [--relay https://relay.example.com] [--no-qr] [--dir PATH]
  devpilot-hook unpair     [--dir PATH]
  devpilot-hook serve      [--dir PATH] [--foreground]
  devpilot-hook doctor     [--json] [--dir PATH]
  devpilot-hook service    install|uninstall|status [--dir PATH]
  devpilot-hook hook       [--gate] <agent>   (invoked from agent config, reads stdin)
  devpilot-hook version
`)
}

// pathsFlag 为子命令注册 --dir 并解析。
func pathsFlag(fs *flag.FlagSet, dir *string) {
	fs.StringVar(dir, "dir", "", "state directory (defaults to the user config directory)")
}

// resolvePaths 返回指定目录或默认目录。
func resolvePaths(dir string) (config.Paths, error) {
	if dir != "" {
		return config.NewPaths(dir), nil
	}
	return config.DefaultPaths()
}

// homeFile 返回用户主目录下的路径。
func homeFile(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cli: cannot locate user home directory: %w", err)
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

// parseAgents 解析逗号分隔的 agent 列表。白名单 = agent 档案表
// (agent.Names(),opencode 是插件形态也在此列)。
func parseAgents(s string) ([]string, error) {
	known := make(map[string]bool, 8)
	for _, n := range agent.Names() {
		known[n] = true
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
			continue
		case known[part]:
			out = append(out, part)
		default:
			return nil, fmt.Errorf("unknown agent: %s (supported: %s)", part, strings.Join(agent.Names(), ", "))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("select at least one agent")
	}
	return out, nil
}
