package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
)

// hookMarker 用于识别我们写入的 hook 条目，卸载时只删自己的。
const hookMarker = "devpilot-hook hook"

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type hookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

// flatCommand 是 Cursor 形的事件条目:命令直接躺在数组里,没有
// matcher/hooks 包装;timeout 单位秒。
type flatCommand struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// copilotCommand 是 Copilot 形的事件条目:type + command(unix)+
// timeoutSec(默认 30,必须显式调大);输出 JSON 单行是它的纪律。
type copilotCommand struct {
	Type       string `json:"type"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

// hookTimeout 按档案的超时单位换算等待窗(秒):gemini 是毫秒、qwen 的
// 值 ≥1000 会被兼容读作毫秒——两者都必须写毫秒,否则 1860 会被读成
// 1.86 秒,30 分钟等待窗当场失效。
func hookTimeout(prof agent.Profile) int {
	seconds := config.DefaultApprovalTimeoutSeconds + 60
	switch prof.HookFile.TimeoutUnit {
	case agent.TimeoutMilliseconds, agent.TimeoutLegacyMSOver1000:
		return seconds * 1000
	default:
		return seconds
	}
}

// commandLine 生成稳定的 hook 命令;路径含空格时加引号。闸门型 agent
// 的 pre-tool-use 模式带 --gate:客户端不读配置也知道要 fail-closed。
func commandLine(exe, agentName string, gate bool) string {
	if strings.ContainsAny(exe, " \t") {
		exe = `"` + exe + `"`
	}
	if gate {
		return exe + " hook --gate " + agentName
	}
	return exe + " hook " + agentName
}

// wantsGate 判断该 agent 在当前模式下是否是闸门:GateOnly(Gemini/Grok,
// 无原生弹窗可回落)且处于 pre-tool-use 模式。
func wantsGate(prof agent.Profile, mode config.ApprovalMode) bool {
	return prof.GateOnly && mode == config.ApprovalModePreToolUse
}

// desiredHookEvents 返回某个 Agent 在当前模式下需要注册的事件。
//
// claude / copilot 在 pre-tool-use 模式下丢弃 PermissionRequest(通道
// 换成 PreToolUse,双注册会一调用两张卡);其余 agent 的通道不受模式
// 影响,事件表保持全集。
func desiredHookEvents(prof agent.Profile, mode config.ApprovalMode) []agent.EventSpec {
	out := make([]agent.EventSpec, 0, len(prof.Events))
	for _, ev := range prof.Events {
		if ev.Event == "PermissionRequest" && prof.DropPRInPreToolUse && mode != config.ApprovalModePermissionRequest {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// installAgentHooks 把 devpilot-hook 条目装进 Agent 的 hooks 配置。
func installAgentHooks(name, exe string, mode config.ApprovalMode) (bool, error) {
	prof, ok := agent.Lookup(name)
	if !ok || prof.HookFile == nil {
		return false, fmt.Errorf("unknown agent: %s", name)
	}
	path, err := prof.HookFile.Path()
	if err != nil {
		return false, err
	}
	if prof.HookFile.Ownership == agent.OwnershipOwned {
		return writeOwnedHooksFile(path, prof, exe, mode)
	}
	return mergeHooksFile(path, prof, exe, mode, true)
}

// uninstallAgentHooks 只删除自家条目;owned 文件整文件删除。
func uninstallAgentHooks(name string) (bool, error) {
	prof, ok := agent.Lookup(name)
	if !ok || prof.HookFile == nil {
		return false, fmt.Errorf("unknown agent: %s", name)
	}
	path, err := prof.HookFile.Path()
	if err != nil {
		return false, err
	}
	if prof.HookFile.Ownership == agent.OwnershipOwned {
		return removeOwnedHooksFile(path, string(prof.Wire))
	}
	return mergeHooksFile(path, prof, "", "", false)
}

// agentConfigPath 返回 Agent hooks 配置文件路径(doctor 用)。
func agentConfigPath(agentName string) (string, error) {
	prof, ok := agent.Lookup(agentName)
	if !ok || prof.HookFile == nil {
		return "", fmt.Errorf("unknown agent: %s", agentName)
	}
	return prof.HookFile.Path()
}

// writeOwnedHooksFile 整文件写入自家的 hooks 配置(grok / copilot):
// 内容一致时不落盘(幂等);目标已存在且不含自家 marker 时拒绝覆盖
// (绝不吞掉别人的文件)。卸载见 removeOwnedHooksFile。
func writeOwnedHooksFile(path string, prof agent.Profile, exe string, mode config.ApprovalMode) (bool, error) {
	root := map[string]json.RawMessage{}
	if prof.HookFile.Format == agent.FormatCopilot {
		root["version"] = json.RawMessage("1")
	}
	timeout := hookTimeout(prof)
	gate := wantsGate(prof, mode)
	hooks := map[string][]json.RawMessage{}
	for _, ev := range desiredHookEvents(prof, mode) {
		var entry any
		switch prof.HookFile.Format {
		case agent.FormatCopilot:
			entry = copilotCommand{Type: "command", Command: commandLine(exe, string(prof.Wire), gate), TimeoutSec: timeout}
		default:
			entry = hookMatcher{
				Matcher: ev.Matcher,
				Hooks: []hookCommand{{
					Type:    "command",
					Command: commandLine(exe, string(prof.Wire), gate),
					Timeout: timeout,
				}},
			}
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return false, err
		}
		hooks[ev.Event] = []json.RawMessage{data}
	}
	data, err := json.Marshal(map[string]any{"hooks": hooks})
	if err != nil {
		return false, err
	}
	// 根对象序列化(version + hooks 有序稳定?Go map 序列化按键排序,
	// version < hooks 字典序——version 在前,形态稳定)。
	rootData, err := json.MarshalIndent(mergeRawMaps(root, data), "", "  ")
	if err != nil {
		return false, err
	}
	rootData = append(rootData, '\n')

	original, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if exists {
		if bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(rootData)) {
			return false, nil
		}
		if !containsAnyMarker(original, string(prof.Wire)) {
			return false, fmt.Errorf("%s exists and is not managed by devpilot-hook; refusing to overwrite", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if err := config.WriteAtomic(path, rootData, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// mergeRawMaps 把 hooks JSON 并进根对象的原始键(owned 文件的 version)。
func mergeRawMaps(root map[string]json.RawMessage, hooksJSON []byte) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(root)+1)
	for k, v := range root {
		out[k] = v
	}
	out["hooks"] = hooksJSON
	return out
}

// removeOwnedHooksFile 删除自家 owned 文件:含自家 marker 才删,外人文件
// 不动。
func removeOwnedHooksFile(path, agentName string) (bool, error) {
	original, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if !containsAnyMarker(original, agentName) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("failed to remove %s: %w", path, err)
	}
	return true, nil
}

// markerFor 是条目级 marker(owned 文件用整文件 marker)。
func markerFor(agentName string) string {
	return hookMarker + " " + agentName
}

// entryMarkers 是识别自家条目的全部命令串:普通安装与闸门安装(--gate
// 中缀)各一条,识别/卸载/doctor 必须两条都认,否则切模式后旧条目孤儿化。
func entryMarkers(agentName string) []string {
	return []string{
		hookMarker + " " + agentName,
		hookMarker + " --gate " + agentName,
	}
}

// containsAnyMarker 判断内容里是否出现任一自家 marker。
func containsAnyMarker(content []byte, agentName string) bool {
	for _, m := range entryMarkers(agentName) {
		if bytes.Contains(content, []byte(m)) {
			return true
		}
	}
	return false
}

// mergeHooksFile 把 devpilot-hook 条目合并进 Agent 的 hooks 配置文件。
//
// 只增改自家条目：其余 key、其余 hook 条目按原始 JSON 保留；
// 解析失败时拒绝写入，避免破坏用户配置。条目形状按档案格式渲染
// (nested / flat / copilot),marker 识别统一按「原始 JSON 含
// devpilot-hook hook <agent>」——三种格式都把命令串原样写进条目。
func mergeHooksFile(path string, prof agent.Profile, exe string, mode config.ApprovalMode, install bool) (bool, error) {
	root := map[string]json.RawMessage{}
	original, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if exists && len(bytes.TrimSpace(original)) > 0 {
		if err := json.Unmarshal(original, &root); err != nil {
			return false, fmt.Errorf("%s is not valid JSON; refusing to modify: %w", path, err)
		}
	}

	hooks := map[string][]json.RawMessage{}
	if raw, ok := root["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return false, fmt.Errorf("malformed hooks field in %s; refusing to modify: %w", path, err)
		}
	}

	marker := string(prof.Wire)
	desired := make(map[string]bool)
	for _, ev := range desiredHookEvents(prof, mode) {
		desired[ev.Event] = true
	}

	gate := wantsGate(prof, mode)
	timeout := hookTimeout(prof)
	for _, ev := range prof.Events {
		kept := make([]json.RawMessage, 0, len(hooks[ev.Event])+1)
		for _, entry := range hooks[ev.Event] {
			if !isOurEntry(entry, marker) {
				kept = append(kept, entry)
			}
		}
		if install && desired[ev.Event] {
			entry, err := renderEntry(prof, ev, commandLine(exe, string(prof.Wire), gate), timeout)
			if err != nil {
				return false, err
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(hooks, ev.Event)
		} else {
			hooks[ev.Event] = kept
		}
	}

	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		data, err := json.Marshal(hooks)
		if err != nil {
			return false, err
		}
		root["hooks"] = data
	}

	// Cursor 形需要顶层 version 键;已有文件不动用户的 version,缺了才补。
	if prof.HookFile.Format == agent.FormatFlat {
		if _, ok := root["version"]; !ok {
			root["version"] = json.RawMessage("1")
		}
	}

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	if exists && bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(data)) {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if exists {
		bak := path + ".devpilot.bak"
		if err := os.WriteFile(bak, original, 0o600); err != nil {
			return false, fmt.Errorf("failed to back up %s: %w", path, err)
		}
	}
	if err := config.WriteAtomic(path, data, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// renderEntry 按档案格式渲染一条事件条目。
func renderEntry(prof agent.Profile, ev agent.EventSpec, command string, timeout int) (json.RawMessage, error) {
	switch prof.HookFile.Format {
	case agent.FormatFlat:
		// Cursor:命令直接躺在事件数组,matcher 字段它自己也支持,但
		// 保持最小形态。
		return json.Marshal(flatCommand{Command: command, Timeout: timeout})
	case agent.FormatCopilot:
		return json.Marshal(copilotCommand{Type: "command", Command: command, TimeoutSec: timeout})
	default:
		return json.Marshal(hookMatcher{
			Matcher: ev.Matcher,
			Hooks: []hookCommand{{
				Type:    "command",
				Command: command,
				Timeout: timeout,
			}},
		})
	}
}

// isOurEntry 判断一条条目是否由本工具写入:原始 JSON 含任一自家 marker
// (普通安装 / --gate 闸门安装)即认——三种格式的条目都把命令串原样写进
// JSON,嵌套形状无关。
func isOurEntry(raw json.RawMessage, agentName string) bool {
	return containsAnyMarker(raw, agentName)
}

// findStableExecutable 优先返回不在版本目录里的稳定路径。
//
// Homebrew 的 bin 链接在升级后继续有效；直接使用 Cellar/Caskroom
// 版本目录里的真实路径会在升级后失效。
func findStableExecutable() (string, error) {
	if p, err := exec.LookPath("devpilot-hook"); err == nil && !isVersionedPath(p) {
		// 保留 bin 下的符号链接本身，不要 EvalSymlinks 到 Cellar 版本目录。
		return p, nil
	}
	// launchd/systemd 拉起的 serve PATH 很窄，LookPath 往往失败；
	// argv[0] 与常见 bin 目录是同一份稳定链接的可靠兜底。
	if p := os.Args[0]; filepath.IsAbs(p) && !isVersionedPath(p) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/opt/homebrew/bin/devpilot-hook",
		"/usr/local/bin/devpilot-hook",
		filepath.Join(home, ".local", "bin", "devpilot-hook"),
	} {
		if p == "" || isVersionedPath(p) {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if isVersionedPath(exe) {
		return "", fmt.Errorf("executable is in a versioned directory %s; run install from a stable Homebrew path (e.g. /opt/homebrew/bin/devpilot-hook)", exe)
	}
	return exe, nil
}

// isVersionedPath 判断路径是否落在会随升级消失的版本目录中。
func isVersionedPath(p string) bool {
	slashed := filepath.ToSlash(p)
	return strings.Contains(slashed, "/Cellar/") || strings.Contains(slashed, "/Caskroom/")
}
